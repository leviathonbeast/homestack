package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"homestack/internal/state"
)

// ---------- Who is connecting ----------

// clientIP returns the real client address. X-Forwarded-For is only trusted
// when the direct peer is a proxy listed in HOMESTACK_TRUSTED_PROXIES.
func (s *Server) clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !s.isTrustedProxy(peer) {
		return peer
	}
	// Walk X-Forwarded-For from the right, skipping our own proxies.
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for _, part := range slices.Backward(parts) {
		a, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !s.isTrustedProxy(a) {
			return a
		}
	}
	return peer
}

func (s *Server) isTrustedProxy(a netip.Addr) bool {
	for _, n := range s.cfg.TrustedProxies {
		if n.Contains(a.AsSlice()) {
			return true
		}
	}
	return false
}

// localIP is the address of ours the browser connected to.
func localIP(r *http.Request) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		host, _, err := net.SplitHostPort(a.String())
		if err == nil {
			if ip, err := netip.ParseAddr(host); err == nil {
				return ip.Unmap().String()
			}
		}
	}
	return ""
}

func ipAllowed(ip netip.Addr, networks []state.Network) bool {
	if len(networks) == 0 {
		return true // an empty list (only possible by hand-editing) allows everyone
	}
	for _, n := range networks {
		p, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			continue
		}
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------- Middleware ----------

// checkAccess rejects anyone outside the allowed networks before they even
// see the login page. The health check stays open for Docker's healthcheck.
func (s *Server) checkAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		ip := s.clientIP(r)
		var nets []state.Network
		s.store.Read(func(st *state.State) { nets = append(nets, st.Settings.AllowedNetworks...) })
		if !ip.IsValid() || !ipAllowed(ip, nets) {
			log.Printf("blocked request from %s (not in allowed networks)", ip)
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, http.StatusForbidden, fmt.Sprintf("Access from %s isn't allowed.", ip))
				return
			}
			http.Error(w, fmt.Sprintf("Access from %s isn't allowed by Homestack's network settings.", ip), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkCSRF requires a custom header on every state-changing API call.
// Browsers won't send custom headers cross-site without a CORS preflight,
// which Homestack never approves, so forged requests from other sites fail.
// This works alongside the SameSite=Strict session cookie.
func (s *Server) checkCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Homestack") != "1" {
				writeErr(w, http.StatusForbidden, "Missing request header.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------- Safe access changes ----------

// pendingAccess holds a network change that hasn't been confirmed yet. If the
// browser can't confirm it within the time limit (because the change cut it
// off), the old settings come back automatically.
type pendingAccess struct {
	mu       sync.Mutex
	token    string
	deadline time.Time
	previous struct {
		networks []state.Network
		listen   []string
	}
	timer *time.Timer
}

const accessConfirmWindow = 60 * time.Second

type accessRequest struct {
	AllowedNetworks []state.Network `json:"allowedNetworks"`
	ListenAddrs     []string        `json:"listenAddrs"`
}

func (s *Server) getAccess(w http.ResponseWriter, r *http.Request) {
	set := s.store.Settings()
	s.access.mu.Lock()
	pending := s.access.token != ""
	var remaining int
	if pending {
		remaining = int(time.Until(s.access.deadline).Seconds())
	}
	s.access.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"allowedNetworks": set.AllowedNetworks,
		"listenAddrs":     set.ListenAddrs,
		"listeners":       s.listeners.Status(),
		"interfaces":      interfaces(),
		"port":            s.cfg.Port,
		"clientIP":        s.clientIP(r).String(),
		"localIP":         localIP(r),
		"pending":         pending,
		"pendingSeconds":  remaining,
		"trustedProxies":  len(s.cfg.TrustedProxies) > 0,
	})
}

// validateAccess checks the new rules and makes sure they won't lock out the
// browser that's making the change.
func (s *Server) validateAccess(r *http.Request, req *accessRequest) error {
	if len(req.AllowedNetworks) == 0 {
		return fmt.Errorf("Add at least one allowed network.")
	}
	for i, n := range req.AllowedNetworks {
		n.Name = strings.TrimSpace(n.Name)
		n.CIDR = strings.TrimSpace(n.CIDR)
		// Accept a bare IP as a single-address network.
		if !strings.Contains(n.CIDR, "/") {
			if ip, err := netip.ParseAddr(n.CIDR); err == nil {
				n.CIDR = netip.PrefixFrom(ip, ip.BitLen()).String()
			}
		}
		p, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			return fmt.Errorf("%q isn't a valid network. Use a range like 192.168.1.0/24.", n.CIDR)
		}
		n.CIDR = p.Masked().String()
		if n.Name == "" {
			n.Name = n.CIDR
		}
		req.AllowedNetworks[i] = n
	}
	for i, a := range req.ListenAddrs {
		ip, err := netip.ParseAddr(strings.TrimSpace(a))
		if err != nil {
			return fmt.Errorf("%q isn't a valid IP address.", a)
		}
		req.ListenAddrs[i] = ip.String()
	}
	client := s.clientIP(r)
	if !ipAllowed(client, req.AllowedNetworks) {
		return fmt.Errorf("You're connected from %s, which these networks don't include. Add it first so you don't lock yourself out.", client)
	}
	if len(req.ListenAddrs) > 0 {
		local := localIP(r)
		found := false
		for _, a := range req.ListenAddrs {
			if a == local {
				found = true
			}
		}
		if !found && local != "" {
			return fmt.Errorf("You're connected through %s. Keep it enabled so you don't lock yourself out.", local)
		}
	}
	return nil
}

func (s *Server) putAccess(w http.ResponseWriter, r *http.Request) {
	var req accessRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateAccess(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.access.mu.Lock()
	defer s.access.mu.Unlock()
	if s.access.token != "" {
		writeErr(w, http.StatusConflict, "Confirm or cancel the previous change first.")
		return
	}
	old := s.store.Settings()
	err := s.store.Update(func(st *state.State) error {
		st.Settings.AllowedNetworks = req.AllowedNetworks
		st.Settings.ListenAddrs = req.ListenAddrs
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save settings.")
		return
	}

	listenChanged := strings.Join(old.ListenAddrs, ",") != strings.Join(req.ListenAddrs, ",")
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	s.access.token = hex.EncodeToString(tok)
	s.access.deadline = time.Now().Add(accessConfirmWindow)
	s.access.previous.networks = old.AllowedNetworks
	s.access.previous.listen = old.ListenAddrs
	s.access.timer = time.AfterFunc(accessConfirmWindow, s.revertAccess)

	writeJSON(w, http.StatusOK, map[string]any{"token": s.access.token, "seconds": int(accessConfirmWindow.Seconds())})
	if listenChanged {
		// Apply after the response has gone out, since this request may be
		// on a listener that's about to be replaced.
		go func(addrs []string) {
			time.Sleep(300 * time.Millisecond)
			s.listeners.Reconcile(addrs)
		}(req.ListenAddrs)
	}
}

func (s *Server) revertAccess() {
	s.revertAccessReason("network settings weren't confirmed in time, so the previous settings were restored")
}

func (s *Server) revertAccessReason(reason string) {
	s.access.mu.Lock()
	defer s.access.mu.Unlock()
	if s.access.token == "" {
		return
	}
	prev := s.access.previous
	_ = s.store.Update(func(st *state.State) error {
		st.Settings.AllowedNetworks = prev.networks
		st.Settings.ListenAddrs = prev.listen
		return nil
	})
	s.access.token = ""
	log.Print(reason)
	go s.listeners.Reconcile(prev.listen)
}

func (s *Server) confirmAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	_ = readJSON(r, &req)
	s.access.mu.Lock()
	defer s.access.mu.Unlock()
	if s.access.token == "" || req.Token != s.access.token {
		writeErr(w, http.StatusConflict, "There's no change waiting to be confirmed. It may have already been reverted.")
		return
	}
	s.access.timer.Stop()
	s.access.token = ""
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) cancelAccess(w http.ResponseWriter, r *http.Request) {
	s.access.mu.Lock()
	if s.access.timer != nil {
		s.access.timer.Stop()
	}
	s.access.mu.Unlock()
	s.revertAccessReason("network settings change undone")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- Interfaces ----------

type ifaceInfo struct {
	Name  string   `json:"name"`
	Addrs []string `json:"addrs"`
	Kind  string   `json:"kind"` // lan, vpn, docker, loopback, other
}

func interfaces() []ifaceInfo {
	out := []ifaceInfo{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := i.Addrs()
		info := ifaceInfo{Name: i.Name, Kind: ifaceKind(i)}
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				if p.Addr().IsLinkLocalUnicast() {
					continue
				}
				info.Addrs = append(info.Addrs, p.Addr().String())
			}
		}
		if len(info.Addrs) == 0 {
			continue
		}
		// Container veth pairs are noise.
		if strings.HasPrefix(i.Name, "veth") {
			continue
		}
		out = append(out, info)
	}
	return out
}

func ifaceKind(i net.Interface) string {
	n := i.Name
	switch {
	case i.Flags&net.FlagLoopback != 0:
		return "loopback"
	case strings.HasPrefix(n, "wg"), strings.HasPrefix(n, "tailscale"), strings.HasPrefix(n, "tun"),
		strings.HasPrefix(n, "zt"), strings.HasPrefix(n, "nebula"), strings.HasPrefix(n, "netbird"):
		return "vpn"
	case strings.HasPrefix(n, "docker"), strings.HasPrefix(n, "br-"):
		return "docker"
	case strings.HasPrefix(n, "eth"), strings.HasPrefix(n, "en"), strings.HasPrefix(n, "wl"), strings.HasPrefix(n, "bond"), strings.HasPrefix(n, "vmbr"):
		return "lan"
	}
	return "other"
}
