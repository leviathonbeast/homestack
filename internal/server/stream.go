package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"time"

	"homestack/internal/docker"
)

// Live data (logs, stats, events) is sent with Server-Sent Events: a plain
// HTTP response that stays open, which browsers reconnect automatically.

type sse struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu sync.Mutex
}

func newSSE(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	s := &sse{w: w, f: f}
	s.flush()
	return s
}

func (s *sse) flush() {
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sse) send(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	s.flush()
	return nil
}

// keepAlive stops proxies and browsers from timing out a quiet stream.
func (s *sse) keepAlive(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			_, _ = fmt.Fprint(s.w, ": ping\n\n")
			s.flush()
			s.mu.Unlock()
		}
	}
}

// ---------- Events ----------

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := newSSE(w)
	go out.keepAlive(ctx)
	_ = out.send("ready", map[string]bool{"ok": true})
	err := s.docker.Events(ctx, func(ev docker.Event) {
		_ = out.send("docker", map[string]string{
			"type":    ev.Type,
			"action":  ev.Action,
			"id":      ev.Actor.ID,
			"name":    ev.Actor.Attributes["name"],
			"project": ev.Actor.Attributes[docker.LabelProject],
		})
	})
	if err != nil && ctx.Err() == nil {
		_ = out.send("error", map[string]string{"error": "Lost connection to Docker."})
	}
}

// ---------- Container sets ----------

// containerLister returns the containers a stream should cover. It's called
// repeatedly so containers recreated by a redeploy are picked up.
type containerLister func(ctx context.Context) ([]docker.Container, error)

func (s *Server) stackContainers(name string) containerLister {
	return func(ctx context.Context) ([]docker.Container, error) {
		stacks, err := s.buildStacks(ctx)
		if err != nil {
			return nil, err
		}
		all, err := s.docker.Containers(ctx)
		if err != nil {
			return nil, err
		}
		ids := map[string]bool{}
		for _, st := range stacks {
			if st.Name == name {
				for _, c := range st.Containers {
					ids[c.ID] = true
				}
			}
		}
		var out []docker.Container
		for _, c := range all {
			if ids[c.ID] {
				out = append(out, c)
			}
		}
		return out, nil
	}
}

func (s *Server) singleContainer(id string) containerLister {
	return func(ctx context.Context) ([]docker.Container, error) {
		all, err := s.docker.Containers(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range all {
			if c.ID == id || c.Name() == id || (len(id) >= 12 && len(c.ID) >= len(id) && c.ID[:len(id)] == id) {
				return []docker.Container{c}, nil
			}
		}
		return nil, nil
	}
}

func displayName(c docker.Container) string {
	if svc := c.Labels[docker.LabelService]; svc != "" {
		return svc
	}
	return c.Name()
}

// ---------- Logs ----------

type logEvent struct {
	C string `json:"c"` // container id (short)
	N string `json:"n"` // service or container name
	docker.LogLine
}

func (s *Server) stackLogs(w http.ResponseWriter, r *http.Request) {
	s.streamLogs(w, r, s.stackContainers(r.PathValue("name")))
}

func (s *Server) containerLogs(w http.ResponseWriter, r *http.Request) {
	s.streamLogs(w, r, s.singleContainer(r.PathValue("id")))
}

// sinceArg turns a Docker log timestamp into the `since` argument, one
// nanosecond later so the last line isn't repeated.
func sinceArg(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	t = t.Add(time.Nanosecond)
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}

func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, list containerLister) {
	tail := 300
	if n, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && n >= 0 && n <= 5000 {
		tail = n
	}
	ctx := r.Context()
	out := newSSE(w)
	go out.keepAlive(ctx)

	lines := make(chan logEvent, 1024)
	var mu sync.Mutex
	following := map[string]bool{}
	lastTS := map[string]string{}
	readOnce := map[string]bool{}

	start := func(c docker.Container, follow bool) {
		id := c.ID
		mu.Lock()
		following[id] = true
		since := ""
		t := strconv.Itoa(tail)
		if ts, ok := lastTS[id]; ok {
			since, t = sinceArg(ts), "all"
		}
		readOnce[id] = true
		mu.Unlock()
		name := displayName(c)
		go func() {
			_ = s.docker.FollowLogs(ctx, id, t, since, follow, func(l docker.LogLine) {
				mu.Lock()
				if l.Time != "" {
					lastTS[id] = l.Time
				}
				mu.Unlock()
				select {
				case lines <- logEvent{C: id[:12], N: name, LogLine: l}:
				case <-ctx.Done():
				}
			})
			mu.Lock()
			following[id] = false
			mu.Unlock()
		}()
	}

	scan := func() {
		cs, err := list(ctx)
		if err != nil {
			return
		}
		type cinfo struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}
		infos := []cinfo{}
		for _, c := range cs {
			infos = append(infos, cinfo{c.ID[:12], displayName(c), c.State})
			mu.Lock()
			busy, seen := following[c.ID], readOnce[c.ID]
			mu.Unlock()
			if busy {
				continue
			}
			if c.State == "running" {
				start(c, true)
			} else if !seen {
				start(c, false) // show past logs of stopped containers once
			}
		}
		_ = out.send("containers", infos)
	}

	// Batch lines so a chatty container doesn't cause thousands of tiny writes.
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		scan()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				scan()
			}
		}
	}()
	batch := make([]logEvent, 0, 256)
	flush := time.NewTicker(150 * time.Millisecond)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-lines:
			batch = append(batch, l)
			if len(batch) >= 500 {
				if out.send("logs", batch) != nil {
					return
				}
				batch = batch[:0]
			}
		case <-flush.C:
			if len(batch) > 0 {
				if out.send("logs", batch) != nil {
					return
				}
				batch = batch[:0]
			}
		}
	}
}

// ---------- Stats ----------

func (s *Server) stackStats(w http.ResponseWriter, r *http.Request) {
	s.streamStats(w, r, s.stackContainers(r.PathValue("name")))
}

func (s *Server) containerStats(w http.ResponseWriter, r *http.Request) {
	s.streamStats(w, r, s.singleContainer(r.PathValue("id")))
}

func (s *Server) streamStats(w http.ResponseWriter, r *http.Request, list containerLister) {
	ctx := r.Context()
	out := newSSE(w)
	var mu sync.Mutex
	latest := map[string]docker.Stats{}
	active := map[string]bool{}

	scan := func() {
		cs, err := list(ctx)
		if err != nil {
			return
		}
		present := map[string]bool{}
		for _, c := range cs {
			if c.State != "running" {
				continue
			}
			present[c.ID] = true
			mu.Lock()
			running := active[c.ID]
			if !running {
				active[c.ID] = true
			}
			mu.Unlock()
			if running {
				continue
			}
			id := c.ID
			go func() {
				_ = s.docker.StreamStats(ctx, id, func(st docker.Stats) {
					mu.Lock()
					latest[id] = st
					mu.Unlock()
				})
				mu.Lock()
				delete(active, id)
				delete(latest, id)
				mu.Unlock()
			}()
		}
		mu.Lock()
		for id := range latest {
			if !present[id] {
				delete(latest, id)
			}
		}
		mu.Unlock()
	}

	scan()
	send := time.NewTicker(2 * time.Second)
	rescan := time.NewTicker(5 * time.Second)
	defer send.Stop()
	defer rescan.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-rescan.C:
			scan()
		case <-send.C:
			mu.Lock()
			snap := make(map[string]docker.Stats, len(latest))
			maps.Copy(snap, latest)
			mu.Unlock()
			if out.send("stats", snap) != nil {
				return
			}
		}
	}
}
