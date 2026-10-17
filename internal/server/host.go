package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// The host agent (`homestack agent`) runs on the server itself and listens
// on a unix socket, mounted into Homestack's container at the same path.
// Homestack passes a short list of requests through, behind its own sign-in.

func agentSocket() string {
	if v := os.Getenv("HOMESTACK_AGENT_SOCKET"); v != "" {
		return v
	}
	return "/run/homestack/agent.sock"
}

var agentClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", agentSocket())
		},
	},
}

func (s *Server) agentAvailable() bool {
	_, err := os.Stat(agentSocket())
	return err == nil
}

// proxyAgent forwards one request to the agent and relays its answer.
func (s *Server) proxyAgent(method, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.agentAvailable() {
			writeErr(w, http.StatusServiceUnavailable, "The host service isn't running. Install it with deploy/debian/agent/install.sh.")
			return
		}
		p := path
		if id := r.PathValue("id"); id != "" {
			p = "/v1/jobs/" + id + "/log"
		}
		if q := r.URL.RawQuery; q != "" {
			p += "?" + q
		}
		var body io.Reader
		if method != http.MethodGet {
			// Big enough for a 2 MB text file from the editor; the agent
			// checks each request's own limit.
			b, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
			if err != nil {
				writeErr(w, http.StatusBadRequest, "Couldn't read the request.")
				return
			}
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(r.Context(), method, "http://agent"+p, body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := agentClient.Do(req)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "Couldn't reach the host service: "+err.Error())
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
	}
}

// agentStream has no overall timeout: downloads can take as long as they take.
var agentStream = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", agentSocket())
		},
	},
}

// downloadFromAgent streams a file from the host agent to the browser.
func (s *Server) downloadFromAgent(w http.ResponseWriter, r *http.Request) {
	if !s.agentAvailable() {
		writeErr(w, http.StatusServiceUnavailable, "The host service isn't running.")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://agent/v1/files/download?"+r.URL.RawQuery, nil)
	resp, err := agentStream.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't reach the host service.")
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length", "X-Content-Type-Options"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) hostAvailable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"available": s.agentAvailable()})
}
