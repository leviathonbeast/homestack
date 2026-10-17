package server

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"homestack/internal/agent"
	"homestack/internal/templates"
)

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.metrics.want()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info, err := s.docker.Info(ctx)
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	stacks, _ := s.buildStacks(ctx)
	var views []ContainerView
	if cs, err := s.docker.Containers(ctx); err == nil {
		for _, c := range cs {
			views = append(views, s.containerView(c))
		}
	}
	topCPU, topMem := topContainers(views, 6)
	imgs, _ := s.docker.Images(ctx)
	vols, _ := s.docker.Volumes(ctx)
	nets, _ := s.docker.Networks(ctx)

	var imgSize int64
	for _, im := range imgs {
		imgSize += im.Size
	}
	stackCounts := map[string]int{"running": 0, "partial": 0, "stopped": 0, "inactive": 0}
	managed := 0
	updates := []map[string]string{}
	for _, st := range stacks {
		if st.External {
			continue
		}
		managed++
		stackCounts[st.Status]++
		if st.UpdateAvailable {
			updates = append(updates, map[string]string{"stack": st.Name})
		}
	}
	host, history := s.metrics.host.Latest()
	if host.MemTotal == 0 { // the first sample isn't in yet
		host = s.metrics.host.Sample(s.diskPaths())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"host":    host,
		"history": history,
		"top":     map[string]any{"cpu": topCPU, "mem": topMem},
		"docker": map[string]any{
			"name":          info.Name,
			"version":       info.ServerVersion,
			"os":            info.OperatingSystem,
			"kernel":        info.KernelVersion,
			"arch":          info.Architecture,
			"cpus":          info.NCPU,
			"memTotal":      info.MemTotal,
			"storageDriver": info.Driver,
		},
		"counts": map[string]any{
			"stacks":            managed,
			"stackStatus":       stackCounts,
			"containers":        info.Containers,
			"containersRunning": info.ContainersRunning,
			"containersStopped": info.ContainersStopped,
			"containersPaused":  info.ContainersPaused,
			"images":            len(imgs),
			"imagesSize":        imgSize,
			"volumes":           len(vols),
			"networks":          len(nets),
		},
		"stacks":       stacks,
		"stackUpdates": updates,
		"homestack":    s.cfg.Version,
	})
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, templates.With(s.templateValues(r)))
}

var reTZ = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){0,2}$`)

// templateValues are this server's answers to the templates' blanks: the
// browser's time zone (?tz=), the address apps are opened on, and where the
// shares are and who owns them (the agent's storage layout).
func (s *Server) templateValues(r *http.Request) templates.Values {
	l := agent.DefaultLayout()
	if s.agentAvailable() {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		var v agent.LayoutView
		if _, err := s.agentDo(ctx, agentClient, http.MethodGet, "/v1/storage/layout", nil, &v); err == nil {
			l = v.Layout
		}
		cancel()
	}
	uid, gid := l.OwnerIDs()
	tz := r.URL.Query().Get("tz")
	if len(tz) > 64 || !reTZ.MatchString(tz) {
		tz = "Etc/UTC"
	}
	return templates.Values{TZ: tz, Host: s.appHost(r), Shares: l.Shares, PUID: strconv.Itoa(uid), PGID: strconv.Itoa(gid)}
}

// appHost is the address apps are opened on: Settings → app links, or the
// one the browser reached Homestack on.
func (s *Server) appHost(r *http.Request) string {
	h := s.store.Settings().AppHost
	if h == "" {
		h = r.Host
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
	}
	if h = strings.Trim(h, "[]"); h == "" {
		return "localhost"
	}
	if strings.Contains(h, ":") {
		return "[" + h + "]" // IPv6, for http://[…]:port
	}
	return h
}

// spa serves the built Svelte app. Unknown paths get index.html so that
// client-side routes like /stacks/jellyfin work on refresh.
func (s *Server) spa() http.Handler {
	fileServer := http.FileServerFS(s.web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if st, err := fs.Stat(s.web, p); err == nil && !st.IsDir() {
			if strings.HasPrefix(p, "assets/") {
				// Vite puts a content hash in these filenames, so they never change.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		f, err := s.web.Open("index.html")
		if err != nil {
			http.Error(w, "The web UI wasn't built into this binary. Run `make build`.", http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.Copy(w, f)
	})
}
