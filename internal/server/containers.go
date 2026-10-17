package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"homestack/internal/docker"
)

func (s *Server) listContainers(w http.ResponseWriter, r *http.Request) {
	s.metrics.want()
	cs, err := s.docker.Containers(r.Context())
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	out := make([]ContainerView, 0, len(cs))
	for _, c := range cs {
		out = append(out, s.containerView(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	raw, err := s.docker.ContainerInspect(r.Context(), id)
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	// Also return the list-style summary, which the UI shares with other pages.
	var summary *ContainerView
	if cs, err := s.docker.Containers(r.Context()); err == nil {
		var full struct {
			ID string `json:"Id"`
		}
		_ = json.Unmarshal(raw, &full)
		for _, c := range cs {
			if c.ID == full.ID {
				v := s.containerView(c)
				summary = &v
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "inspect": raw})
}

var containerActions = map[string]bool{
	"start": true, "stop": true, "restart": true, "kill": true, "pause": true, "unpause": true,
}

func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !containerActions[action] {
		writeErr(w, http.StatusBadRequest, "Unknown action.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.docker.ContainerAction(ctx, r.PathValue("id"), action); err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) removeContainer(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	vols := r.URL.Query().Get("volumes") == "1"
	if err := s.docker.ContainerRemove(r.Context(), r.PathValue("id"), force, vols); err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- Images ----------

type imageView struct {
	ID              string   `json:"id"`
	Tags            []string `json:"tags"`
	Created         int64    `json:"created"`
	Size            int64    `json:"size"`
	InUse           int      `json:"inUse"` // containers using it
	UsedBy          []string `json:"usedBy"`
	UpdateAvailable bool     `json:"updateAvailable"`
}

func (s *Server) listImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.docker.Images(r.Context())
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	cs, _ := s.docker.Containers(r.Context())
	users := map[string][]string{}
	for _, c := range cs {
		users[c.ImageID] = append(users[c.ImageID], c.Name())
	}
	out := make([]imageView, 0, len(imgs))
	for _, im := range imgs {
		v := imageView{ID: im.ID, Tags: []string{}, Created: im.Created, Size: im.Size, UsedBy: users[im.ID]}
		for _, t := range im.RepoTags {
			if t != "<none>:<none>" {
				v.Tags = append(v.Tags, t)
				if s.updates.available(t) {
					v.UpdateAvailable = true
				}
			}
		}
		if v.UsedBy == nil {
			v.UsedBy = []string{}
		}
		v.InUse = len(v.UsedBy)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) pullImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image string `json:"image"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ref := strings.TrimSpace(req.Image)
	if ref == "" || strings.ContainsAny(ref, " \t\n") {
		writeErr(w, http.StatusBadRequest, "Enter an image name, like nginx:alpine.")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	out := newNDJSON(w)
	last := map[string]string{}
	err := s.docker.ImagePull(ctx, ref, func(p docker.PullProgress) {
		// Skip the flood of byte-count updates; report each layer's stage once.
		if last[p.ID] == p.Status {
			return
		}
		last[p.ID] = p.Status
		if p.ID != "" {
			out.line(p.ID + ": " + p.Status)
		} else {
			out.line(p.Status)
		}
	})
	if err == nil {
		go s.updates.recheckRefs([]string{ref})
	}
	out.done(err)
}

func (s *Server) removeImage(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.ImageRemove(r.Context(), r.PathValue("id"), r.URL.Query().Get("force") == "1"); err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) pruneImages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		All bool `json:"all"`
	}
	_ = readJSON(r, &req)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rep, err := s.docker.ImagesPrune(ctx, req.All)
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ---------- Volumes ----------

type volumeView struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Mountpoint string   `json:"mountpoint"`
	Created    string   `json:"created"`
	Stack      string   `json:"stack,omitempty"`
	UsedBy     []string `json:"usedBy"`
	Anonymous  bool     `json:"anonymous"`
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	vols, err := s.docker.Volumes(r.Context())
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	cs, _ := s.docker.Containers(r.Context())
	users := map[string][]string{}
	for _, c := range cs {
		for _, m := range c.Mounts {
			if m.Type == "volume" {
				users[m.Name] = append(users[m.Name], c.Name())
			}
		}
	}
	out := make([]volumeView, 0, len(vols))
	for _, v := range vols {
		vv := volumeView{
			Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint, Created: v.CreatedAt,
			Stack: v.Labels[docker.LabelProject], UsedBy: users[v.Name],
			Anonymous: v.Labels["com.docker.volume.anonymous"] != "" || (len(v.Name) == 64 && isHex(v.Name)),
		}
		if vv.UsedBy == nil {
			vv.UsedBy = []string{}
		}
		out = append(out, vv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Anonymous != out[j].Anonymous {
			return !out[i].Anonymous
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "Enter a volume name.")
		return
	}
	if err := s.docker.VolumeCreate(r.Context(), strings.TrimSpace(req.Name)); err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Server) removeVolume(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.VolumeRemove(r.Context(), r.PathValue("name")); err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) pruneVolumes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		All bool `json:"all"`
	}
	_ = readJSON(r, &req)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rep, err := s.docker.VolumesPrune(ctx, req.All)
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ---------- Networks ----------

type networkView struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Scope      string            `json:"scope"`
	Internal   bool              `json:"internal"`
	Subnet     string            `json:"subnet"`
	Gateway    string            `json:"gateway"`
	Stack      string            `json:"stack,omitempty"`
	Containers []string          `json:"containers"`
	IPs        map[string]string `json:"ips"`               // container -> its address on this network
	Parent     string            `json:"parent,omitempty"`  // macvlan/ipvlan: the host interface
	IPRange    string            `json:"ipRange,omitempty"` // the addresses Docker hands out
	System     bool              `json:"system"`            // bridge, host, none: can't be removed
}

func (s *Server) listNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := s.docker.Networks(r.Context())
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	cs, _ := s.docker.Containers(r.Context())
	members := map[string][]string{}
	ips := map[string]map[string]string{}
	for _, c := range cs {
		for _, n := range c.NetworkSettings.Networks {
			members[n.NetworkID] = append(members[n.NetworkID], c.Name())
			if n.IPAddress != "" {
				if ips[n.NetworkID] == nil {
					ips[n.NetworkID] = map[string]string{}
				}
				ips[n.NetworkID][c.Name()] = n.IPAddress
			}
		}
	}
	out := make([]networkView, 0, len(nets))
	for _, n := range nets {
		v := networkView{
			ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Internal: n.Internal,
			Stack: n.Labels[docker.LabelProject], Containers: members[n.ID],
			IPs: ips[n.ID], Parent: n.Options["parent"],
			System: n.Name == "bridge" || n.Name == "host" || n.Name == "none",
		}
		if len(n.IPAM.Config) > 0 {
			v.Subnet, v.Gateway, v.IPRange = n.IPAM.Config[0].Subnet, n.IPAM.Config[0].Gateway, n.IPAM.Config[0].IPRange
		}
		if v.Containers == nil {
			v.Containers = []string{}
		}
		if v.IPs == nil {
			v.IPs = map[string]string{}
		}
		sort.Strings(v.Containers)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].System != out[j].System {
			return !out[i].System
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

type createNetworkReq struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Internal bool   `json:"internal"`
	Parent   string `json:"parent"`
	Subnet   string `json:"subnet"`
	Gateway  string `json:"gateway"`
	IPRange  string `json:"ipRange"`
}

func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	var req createNetworkReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	spec, err := s.dockerNetworkSpec(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.docker.NetworkCreate(r.Context(), spec); err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Server) removeNetwork(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.NetworkRemove(r.Context(), r.PathValue("id")); err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) pruneNetworks(w http.ResponseWriter, r *http.Request) {
	rep, err := s.docker.NetworksPrune(r.Context())
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
