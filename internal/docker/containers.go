package docker

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// Compose labels that link containers to stacks.
const (
	LabelProject    = "com.docker.compose.project"
	LabelService    = "com.docker.compose.service"
	LabelWorkingDir = "com.docker.compose.project.working_dir"
	LabelConfigs    = "com.docker.compose.project.config_files"
)

// Port is one published or exposed port.
type Port struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

// Mount is a volume or bind mount.
type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name,omitempty"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// Container is one row from /containers/json.
type Container struct {
	ID         string            `json:"Id"`
	Names      []string          `json:"Names"`
	Image      string            `json:"Image"`
	ImageID    string            `json:"ImageID"`
	Command    string            `json:"Command"`
	Created    int64             `json:"Created"`
	State      string            `json:"State"`
	Status     string            `json:"Status"`
	Ports      []Port            `json:"Ports"`
	Labels     map[string]string `json:"Labels"`
	Mounts     []Mount           `json:"Mounts"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			NetworkID string `json:"NetworkID"`
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Name returns the container name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:12]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Containers lists all containers (running and stopped).
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var out []Container
	err := c.getJSON(ctx, "GET", "/containers/json", url.Values{"all": {"1"}}, nil, &out)
	return out, err
}

// ContainerInspect returns the full inspect JSON, kept raw so nothing is lost.
func (c *Client) ContainerInspect(ctx context.Context, id string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.getJSON(ctx, "GET", "/containers/"+url.PathEscape(id)+"/json", nil, nil, &raw)
	return raw, err
}

// ContainerTTY reports whether a container was started with a TTY, which
// changes how its log stream is framed.
func (c *Client) ContainerTTY(ctx context.Context, id string) (bool, error) {
	var v struct {
		Config struct {
			Tty bool `json:"Tty"`
		} `json:"Config"`
	}
	err := c.getJSON(ctx, "GET", "/containers/"+url.PathEscape(id)+"/json", nil, nil, &v)
	return v.Config.Tty, err
}

// ContainerAction runs start, stop, restart, kill, pause or unpause.
func (c *Client) ContainerAction(ctx context.Context, id, action string) error {
	q := url.Values{}
	if action == "stop" || action == "restart" {
		q.Set("t", "10")
	}
	err := c.getJSON(ctx, "POST", "/containers/"+url.PathEscape(id)+"/"+action, q, nil, nil)
	// 304 means "already in that state", which is fine.
	if e, ok := err.(*APIError); ok && e.Status == 304 {
		return nil
	}
	return err
}

// ContainerRemove deletes a container. Volumes are kept unless removeVolumes.
func (c *Client) ContainerRemove(ctx context.Context, id string, force, removeVolumes bool) error {
	q := url.Values{"force": {strconv.FormatBool(force)}, "v": {strconv.FormatBool(removeVolumes)}}
	return c.getJSON(ctx, "DELETE", "/containers/"+url.PathEscape(id), q, nil, nil)
}
