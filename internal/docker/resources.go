package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// ---------- Images ----------

// Image is one row from /images/json.
type Image struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     int64    `json:"Created"`
	Size        int64    `json:"Size"`
	Containers  int64    `json:"Containers"`
}

func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var out []Image
	err := c.getJSON(ctx, "GET", "/images/json", nil, nil, &out)
	return out, err
}

// ImageRepoDigests returns the registry digests of a local image.
func (c *Client) ImageRepoDigests(ctx context.Context, ref string) ([]string, error) {
	var v struct {
		RepoDigests []string `json:"RepoDigests"`
	}
	err := c.getJSON(ctx, "GET", "/images/"+ref+"/json", nil, nil, &v)
	return v.RepoDigests, err
}

// RemoteDigest asks the registry (through the daemon, which handles auth) for
// the current digest of an image reference.
func (c *Client) RemoteDigest(ctx context.Context, ref string) (string, error) {
	var v struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	err := c.getJSON(ctx, "GET", "/distribution/"+ref+"/json", nil, nil, &v)
	return v.Descriptor.Digest, err
}

func (c *Client) ImageRemove(ctx context.Context, id string, force bool) error {
	return c.getJSON(ctx, "DELETE", "/images/"+url.PathEscape(id), url.Values{"force": {strconv.FormatBool(force)}}, nil, nil)
}

// PruneReport is what Docker says it removed.
type PruneReport struct {
	Deleted        int    `json:"deleted"`
	SpaceReclaimed uint64 `json:"spaceReclaimed"`
}

// ImagesPrune removes dangling images, or all unused images when all is true.
func (c *Client) ImagesPrune(ctx context.Context, all bool) (PruneReport, error) {
	q := url.Values{}
	if all {
		q.Set("filters", filters(map[string][]string{"dangling": {"false"}}))
	}
	var v struct {
		ImagesDeleted  []any  `json:"ImagesDeleted"`
		SpaceReclaimed uint64 `json:"SpaceReclaimed"`
	}
	err := c.getJSON(ctx, "POST", "/images/prune", q, nil, &v)
	return PruneReport{Deleted: len(v.ImagesDeleted), SpaceReclaimed: v.SpaceReclaimed}, err
}

// PullProgress is one progress message from an image pull.
type PullProgress struct {
	ID       string `json:"id,omitempty"`
	Status   string `json:"status,omitempty"`
	Progress string `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ImagePull pulls an image, reporting progress as it goes.
func (c *Client) ImagePull(ctx context.Context, ref string, fn func(PullProgress)) error {
	image, tag := SplitRef(ref)
	body, err := c.Stream(ctx, "POST", "/images/create", url.Values{"fromImage": {image}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	dec := json.NewDecoder(bufio.NewReader(body))
	for {
		var p struct {
			PullProgress
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&p); err != nil {
			return nil // EOF: done
		}
		if p.Error != "" {
			return &APIError{Status: 500, Message: p.Error}
		}
		fn(p.PullProgress)
	}
}

// SplitRef splits "repo/name:tag" into name and tag (defaulting to latest).
// Digests ("name@sha256:...") are kept whole.
func SplitRef(ref string) (string, string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

// ---------- Volumes ----------

type Volume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
}

func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	var v struct {
		Volumes []Volume `json:"Volumes"`
	}
	err := c.getJSON(ctx, "GET", "/volumes", nil, nil, &v)
	return v.Volumes, err
}

func (c *Client) VolumeCreate(ctx context.Context, name string) error {
	return c.getJSON(ctx, "POST", "/volumes/create", nil, map[string]any{"Name": name}, nil)
}

func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	return c.getJSON(ctx, "DELETE", "/volumes/"+url.PathEscape(name), nil, nil, nil)
}

// VolumesPrune removes unused volumes. Since Docker 23, prune only touches
// anonymous volumes unless all is set.
func (c *Client) VolumesPrune(ctx context.Context, all bool) (PruneReport, error) {
	q := url.Values{}
	if all {
		q.Set("filters", filters(map[string][]string{"all": {"true"}}))
	}
	var v struct {
		VolumesDeleted []string `json:"VolumesDeleted"`
		SpaceReclaimed uint64   `json:"SpaceReclaimed"`
	}
	err := c.getJSON(ctx, "POST", "/volumes/prune", q, nil, &v)
	return PruneReport{Deleted: len(v.VolumesDeleted), SpaceReclaimed: v.SpaceReclaimed}, err
}

// ---------- Networks ----------

type Network struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Scope    string            `json:"Scope"`
	Internal bool              `json:"Internal"`
	Created  string            `json:"Created"`
	Labels   map[string]string `json:"Labels"`
	Options  map[string]string `json:"Options"`
	IPAM     struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
			IPRange string `json:"IPRange"`
		} `json:"Config"`
	} `json:"IPAM"`
}

func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var out []Network
	err := c.getJSON(ctx, "GET", "/networks", nil, nil, &out)
	return out, err
}

// NetworkSpec is a network to create. Parent, IPRange and Gateway matter for
// macvlan and ipvlan (containers on the LAN); Subnet is optional for bridge.
type NetworkSpec struct {
	Name     string
	Driver   string // bridge | macvlan | ipvlan
	Internal bool
	Parent   string
	Subnet   string
	Gateway  string
	IPRange  string
}

func (c *Client) NetworkCreate(ctx context.Context, s NetworkSpec) error {
	if s.Driver == "" {
		s.Driver = "bridge"
	}
	body := map[string]any{"Name": s.Name, "Driver": s.Driver, "Internal": s.Internal, "CheckDuplicate": true}
	opts := map[string]string{}
	if s.Parent != "" {
		opts["parent"] = s.Parent
	}
	if s.Driver == "ipvlan" {
		opts["ipvlan_mode"] = "l2"
	}
	if len(opts) > 0 {
		body["Options"] = opts
	}
	if s.Subnet != "" {
		cfg := map[string]string{"Subnet": s.Subnet}
		if s.Gateway != "" {
			cfg["Gateway"] = s.Gateway
		}
		if s.IPRange != "" {
			cfg["IPRange"] = s.IPRange
		}
		body["IPAM"] = map[string]any{"Driver": "default", "Config": []map[string]string{cfg}}
	}
	return c.getJSON(ctx, "POST", "/networks/create", nil, body, nil)
}

func (c *Client) NetworkRemove(ctx context.Context, id string) error {
	return c.getJSON(ctx, "DELETE", "/networks/"+url.PathEscape(id), nil, nil, nil)
}

func (c *Client) NetworksPrune(ctx context.Context) (PruneReport, error) {
	var v struct {
		NetworksDeleted []string `json:"NetworksDeleted"`
	}
	err := c.getJSON(ctx, "POST", "/networks/prune", nil, nil, &v)
	return PruneReport{Deleted: len(v.NetworksDeleted)}, err
}
