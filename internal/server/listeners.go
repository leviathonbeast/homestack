package server

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// listenerMgr owns the sockets Homestack listens on. The set can change at
// runtime from the settings page, without restarting the process.
//
// If an address can't be bound yet (for example wg0 isn't up at boot), it
// keeps retrying every 10 seconds instead of failing.
type listenerMgr struct {
	mu      sync.Mutex
	port    string
	handler http.Handler
	active  map[string]*listener
}

type listener struct {
	addr   string
	srv    *http.Server
	stop   chan struct{}
	mu     sync.Mutex
	bound  bool
	errMsg string
}

// ListenerStatus is shown on the settings page.
type ListenerStatus struct {
	Addr  string `json:"addr"`
	Bound bool   `json:"bound"`
	Error string `json:"error,omitempty"`
}

func newListenerMgr(port string, h http.Handler) *listenerMgr {
	return &listenerMgr{port: port, handler: h, active: map[string]*listener{}}
}

// addrsFor turns the settings value into host:port pairs. No IPs means every
// interface.
func (m *listenerMgr) addrsFor(ips []string) []string {
	if len(ips) == 0 {
		return []string{net.JoinHostPort("", m.port)}
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip, m.port))
	}
	return out
}

// Reconcile starts and stops listeners so they match ips.
func (m *listenerMgr) Reconcile(ips []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]bool{}
	for _, a := range m.addrsFor(ips) {
		want[a] = true
	}
	for addr, l := range m.active {
		if !want[addr] {
			close(l.stop)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if l.srv.Shutdown(ctx) != nil {
				_ = l.srv.Close() // long-lived streams (logs, stats) don't end on their own
			}
			cancel()
			delete(m.active, addr)
			log.Printf("stopped listening on %s", addr)
		}
	}
	for addr := range want {
		if _, ok := m.active[addr]; ok {
			continue
		}
		l := &listener{
			addr: addr,
			stop: make(chan struct{}),
			srv: &http.Server{
				Handler:           m.handler,
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       120 * time.Second,
			},
		}
		m.active[addr] = l
		go l.run()
	}
}

func (l *listener) run() {
	for {
		ln, err := net.Listen("tcp", l.addr)
		if err == nil {
			l.mu.Lock()
			l.bound, l.errMsg = true, ""
			l.mu.Unlock()
			log.Printf("listening on http://%s", displayAddr(l.addr))
			err = l.srv.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				return
			}
		}
		l.mu.Lock()
		l.bound = false
		if err != nil {
			l.errMsg = err.Error()
		}
		l.mu.Unlock()
		log.Printf("can't listen on %s (%v), retrying in 10s", l.addr, err)
		select {
		case <-l.stop:
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func displayAddr(a string) string {
	host, port, _ := net.SplitHostPort(a)
	if host == "" {
		return "0.0.0.0:" + port
	}
	return a
}

// Status reports each listener.
func (m *listenerMgr) Status() []ListenerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ListenerStatus{}
	for _, l := range m.active {
		l.mu.Lock()
		out = append(out, ListenerStatus{Addr: displayAddr(l.addr), Bound: l.bound, Error: l.errMsg})
		l.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// Shutdown stops every listener.
func (m *listenerMgr) Shutdown(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for addr, l := range m.active {
		close(l.stop)
		if l.srv.Shutdown(ctx) != nil {
			_ = l.srv.Close()
		}
		delete(m.active, addr)
	}
}
