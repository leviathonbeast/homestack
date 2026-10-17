package server

import (
	"context"
	"sort"
	"sync"
	"time"

	"homestack/internal/docker"
	"homestack/internal/sysinfo"
)

// Live figures for the dashboard, Stacks and Containers pages: host metrics
// sampled every 5 seconds (with 15 minutes of history for the sparklines),
// and each running container's CPU and memory, read every 10 seconds while
// someone is looking. Docker's stats cost the daemon a little, so those
// stop when no page has asked for a few minutes.

const (
	hostEvery  = 5 * time.Second
	usageEvery = 10 * time.Second
	usageIdle  = 3 * time.Minute
)

type ctrUsage struct {
	CPU float64
	Mem uint64
}

type metrics struct {
	host *sysinfo.Sampler
	wake chan struct{}

	mu       sync.Mutex
	wanted   time.Time
	readings map[string]docker.Reading
	usage    map[string]ctrUsage // container ID -> latest
}

func newMetrics() *metrics {
	return &metrics{
		host:     sysinfo.NewSampler(""),
		wake:     make(chan struct{}, 1),
		readings: map[string]docker.Reading{},
		usage:    map[string]ctrUsage{},
	}
}

// want says a page is showing container figures, so keep reading them.
func (m *metrics) want() {
	if m == nil {
		return
	}
	m.mu.Lock()
	idle := time.Since(m.wanted) > usageIdle
	m.wanted = time.Now()
	m.mu.Unlock()
	if idle {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

func (m *metrics) usageFor(id string) (ctrUsage, bool) {
	if m == nil {
		return ctrUsage{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.usage[id]
	return u, ok
}

func (s *Server) diskPaths() map[string]string {
	// "/" is the system disk: inside Docker it reports the filesystem that
	// holds Docker's data.
	return map[string]string{"Stacks": s.store.Settings().StacksDir, "System": "/"}
}

func (s *Server) sampleHost(ctx context.Context) {
	t := time.NewTicker(hostEvery)
	defer t.Stop()
	for {
		s.metrics.host.Sample(s.diskPaths())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sampleContainers(ctx context.Context) {
	t := time.NewTicker(usageEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.metrics.wake:
			// Waking up: two quick readings, so CPU figures show straight away.
			s.readUsage(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(1500 * time.Millisecond):
			}
			s.readUsage(ctx)
		case <-t.C:
			s.metrics.mu.Lock()
			active := time.Since(s.metrics.wanted) < usageIdle
			s.metrics.mu.Unlock()
			if active {
				s.readUsage(ctx)
			}
		}
	}
}

// readUsage takes a reading of every running container, a few at a time.
func (s *Server) readUsage(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	list, err := s.docker.Containers(lctx)
	if err != nil {
		return
	}
	type result struct {
		id string
		r  docker.Reading
	}
	var (
		wg      sync.WaitGroup
		resMu   sync.Mutex
		results []result
		sem     = make(chan struct{}, 4)
	)
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := s.docker.StatsOnce(lctx, c.ID)
			if err != nil {
				return
			}
			resMu.Lock()
			results = append(results, result{c.ID, r})
			resMu.Unlock()
		})
	}
	wg.Wait()

	m := s.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	readings, usage := map[string]docker.Reading{}, map[string]ctrUsage{}
	for _, res := range results {
		u := ctrUsage{Mem: res.r.Mem}
		if prev, ok := m.readings[res.id]; ok {
			u.CPU = docker.CPUBetween(prev, res.r)
		}
		readings[res.id], usage[res.id] = res.r, u
	}
	m.readings, m.usage = readings, usage // containers that stopped drop out
}

// TopContainer is one row of the dashboard's busiest containers.
type TopContainer struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Stack string  `json:"stack,omitempty"`
	CPU   float64 `json:"cpu"`
	Mem   uint64  `json:"mem"`
}

// topContainers returns the n busiest running containers by CPU and by memory.
func topContainers(views []ContainerView, n int) (byCPU, byMem []TopContainer) {
	var all []TopContainer
	for _, c := range views {
		if c.CPU != nil && c.Mem != nil {
			all = append(all, TopContainer{ID: c.ID, Name: c.Name, Stack: c.Stack, CPU: *c.CPU, Mem: *c.Mem})
		}
	}
	pick := func(less func(a, b TopContainer) bool) []TopContainer {
		out := append([]TopContainer{}, all...)
		sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
		return out[:min(n, len(out))]
	}
	return pick(func(a, b TopContainer) bool { return a.CPU > b.CPU }), pick(func(a, b TopContainer) bool { return a.Mem > b.Mem })
}
