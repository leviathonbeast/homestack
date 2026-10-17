// Package docker is a small client for the Docker Engine API.
//
// The official SDK pulls in a large dependency tree, and Homestack only needs
// a few dozen endpoints, so this talks HTTP over the Docker socket directly.
// Compose operations shell out to the `docker compose` CLI (see compose.go).
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client talks to one Docker daemon.
type Client struct {
	network string // "unix" or "tcp"
	address string
	http    *http.Client // for normal requests (with timeout)
	stream  *http.Client // for long-lived streams (no timeout)
}

// New connects using DOCKER_HOST if set, otherwise the default socket.
// Supported forms: unix:///path/to/docker.sock and tcp://host:port (e.g. a
// docker-socket-proxy).
func New() (*Client, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	c := &Client{}
	switch {
	case strings.HasPrefix(host, "unix://"):
		c.network, c.address = "unix", strings.TrimPrefix(host, "unix://")
	case strings.HasPrefix(host, "tcp://"):
		c.network, c.address = "tcp", strings.TrimPrefix(host, "tcp://")
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST %q (use unix:// or tcp://)", host)
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, c.network, c.address)
	}
	c.http = &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{DialContext: dial, MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second},
	}
	c.stream = &http.Client{
		Transport: &http.Transport{DialContext: dial, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second},
	}
	return c, nil
}

// APIError is an error response from the daemon.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// IsNotFound reports whether err is a 404 from Docker.
func IsNotFound(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == http.StatusNotFound
	}
	return false
}

func (c *Client) do(ctx context.Context, client *http.Client, method, path string, query url.Values, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = strings.NewReader(string(b))
	}
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach Docker: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct {
			Message string `json:"message"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(data, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		if e.Message == "" {
			e.Message = resp.Status
		}
		return nil, &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	return resp, nil
}

// getJSON does a request and decodes the JSON response into out.
func (c *Client) getJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, c.http, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream opens a long-lived response body (logs, stats, events, pulls).
func (c *Client) Stream(ctx context.Context, method, path string, query url.Values, body any) (io.ReadCloser, error) {
	resp, err := c.do(ctx, c.stream, method, path, query, body)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Ping checks the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	return c.getJSON(ctx, "GET", "/_ping", nil, nil, nil)
}

// filters encodes Docker's JSON filter syntax.
func filters(f map[string][]string) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// ---------- System ----------

// Info is the subset of /info the dashboard shows.
type Info struct {
	Name              string `json:"Name"`
	ServerVersion     string `json:"ServerVersion"`
	OperatingSystem   string `json:"OperatingSystem"`
	OSType            string `json:"OSType"`
	Architecture      string `json:"Architecture"`
	KernelVersion     string `json:"KernelVersion"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersPaused  int    `json:"ContainersPaused"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
	DockerRootDir     string `json:"DockerRootDir"`
	Driver            string `json:"Driver"`
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.getJSON(ctx, "GET", "/info", nil, nil, &i)
	return i, err
}

// Event is one line from the Docker events stream.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time int64 `json:"time"`
}

// Events streams container, image, volume and network events until ctx ends.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	q := url.Values{"filters": {filters(map[string][]string{"type": {"container", "image", "volume", "network"}})}}
	body, err := c.Stream(ctx, "GET", "/events", q, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	dec := json.NewDecoder(bufio.NewReader(body))
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			return err
		}
		fn(ev)
	}
}
