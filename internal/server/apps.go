package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"homestack/internal/docker"
	"homestack/internal/state"
)

// App links: dashboard shortcuts to each app's web page. Links are saved by
// the admin; suggestions are worked out from each container's labels,
// published ports, or (for host-networked containers) the app's usual port.

const hostToken = "[HOST]"

// knownPorts maps an image name fragment to the port (and path) its web UI
// normally uses. Used for containers on host networking, which publish no
// ports, and to pick the web port when a container publishes several.
var knownPorts = []struct {
	match string
	port  int
	path  string
}{
	{"sabnzbd", 8080, "/"}, {"nzbget", 6789, "/"},
	{"sonarr", 8989, "/"}, {"radarr", 7878, "/"}, {"lidarr", 8686, "/"}, {"readarr", 8787, "/"},
	{"prowlarr", 9696, "/"}, {"bazarr", 6767, "/"}, {"overseerr", 5055, "/"}, {"jellyseerr", 5055, "/"},
	{"tautulli", 8181, "/"}, {"jellyfin", 8096, "/"}, {"emby", 8096, "/"}, {"plex", 32400, "/web"},
	{"navidrome", 4533, "/"}, {"audiobookshelf", 80, "/"},
	{"qbittorrent", 8080, "/"}, {"transmission", 9091, "/"}, {"deluge", 8112, "/"},
	{"nicotine", 6080, "/"}, {"metube", 8081, "/"},
	{"makemkv", 5800, "/"}, {"mkvtoolnix", 5800, "/"}, {"picard", 5800, "/"}, {"tinymediamanager", 4000, "/"},
	{"resilio", 8888, "/"}, {"syncthing", 8384, "/"},
	{"gitea", 3000, "/"}, {"forgejo", 3000, "/"}, {"vaultwarden", 80, "/"},
	{"homeassistant", 8123, "/"}, {"home-assistant", 8123, "/"},
	{"uptime-kuma", 3001, "/"}, {"portainer", 9000, "/"}, {"pihole", 80, "/admin/"},
	{"adguardhome", 3000, "/"}, {"nginx-proxy-manager", 81, "/"}, {"paperless", 8000, "/"},
	{"immich", 2283, "/"}, {"nextcloud", 80, "/"}, {"grafana", 3000, "/"}, {"dawarich", 3000, "/"},
	{"filebrowser", 80, "/"}, {"code-server", 8443, "/"}, {"homepage", 3000, "/"},
}

// Ports that are never a web page, so they're not offered as links.
var notWebPorts = map[int]bool{21: true, 22: true, 25: true, 53: true, 110: true, 143: true, 389: true, 445: true,
	1883: true, 1900: true, 3306: true, 5432: true, 6379: true, 11211: true, 27017: true, 51820: true, 51413: true}

var webPorts = map[int]bool{80: true, 443: true, 3000: true, 5000: true, 8000: true, 8080: true, 8081: true, 8443: true, 8888: true, 9000: true}

var unraidPort = regexp.MustCompile(`\[PORT:(\d+)\]`)

// webURL works out a container's web address, or "" if it has none.
func webURL(c docker.Container) string {
	hostNet := c.HostConfig.NetworkMode == "host"
	published := func(private int) int {
		for _, p := range c.Ports {
			if p.Type == "tcp" && p.PrivatePort == private && p.PublicPort != 0 {
				return p.PublicPort
			}
		}
		if hostNet {
			return private
		}
		return 0
	}
	// 1. A label, in Homestack's or Unraid's template style: http://[IP]:[PORT:8080]/
	for _, key := range []string{"homestack.webui", "net.unraid.docker.webui"} {
		if tpl := strings.TrimSpace(c.Labels[key]); tpl != "" {
			ok := true
			u := unraidPort.ReplaceAllStringFunc(tpl, func(m string) string {
				n, _ := strconv.Atoi(unraidPort.FindStringSubmatch(m)[1])
				if pub := published(n); pub != 0 {
					return strconv.Itoa(pub)
				}
				ok = false
				return m
			})
			u = strings.NewReplacer("[IP]", hostToken, "[HOST]", hostToken).Replace(u)
			if ok && validAppURL(u) == nil {
				return u
			}
		}
	}
	scheme := func(port int) string {
		if port == 443 || port == 8443 || port == 9443 {
			return "https"
		}
		return "http"
	}
	// 2. The app's usual port, if we recognise the image.
	img := strings.ToLower(c.Image)
	for _, k := range knownPorts {
		if strings.Contains(img, k.match) {
			if pub := published(k.port); pub != 0 {
				return fmt.Sprintf("%s://%s:%d%s", scheme(k.port), hostToken, pub, k.path)
			}
		}
	}
	// 3. A published port that looks like a web page.
	var candidates []docker.Port
	for _, p := range c.Ports {
		if p.Type == "tcp" && p.PublicPort != 0 && !notWebPorts[p.PrivatePort] {
			candidates = append(candidates, p)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].PrivatePort < candidates[j].PrivatePort })
	for _, p := range candidates {
		if webPorts[p.PrivatePort] {
			return fmt.Sprintf("%s://%s:%d/", scheme(p.PrivatePort), hostToken, p.PublicPort)
		}
	}
	if len(candidates) > 0 {
		p := candidates[0]
		return fmt.Sprintf("%s://%s:%d/", scheme(p.PrivatePort), hostToken, p.PublicPort)
	}
	return ""
}

// validAppURL accepts http(s) URLs, with [HOST] standing in for the host.
func validAppURL(raw string) error {
	if len(raw) > 500 {
		return errors.New("That address is too long.")
	}
	u, err := url.Parse(strings.ReplaceAll(raw, hostToken, "homestack-host"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("Use a web address starting with http:// or https://, for example http://[HOST]:8080/.")
	}
	return nil
}

func prettyName(c docker.Container) string {
	name := c.Name()
	if svc := c.Labels[docker.LabelService]; svc != "" {
		name = svc
	}
	return name
}

func firstName(c docker.Container) string { return c.Name() }

func newAppID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type appView struct {
	state.AppLink
	State string `json:"state"` // running, exited… or "" when not linked to a container
}

type appSuggestion struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Container string `json:"container"`
	Image     string `json:"image"`
	State     string `json:"state"`
}

// appLinkFor returns the saved link for a container, if there is one.
func (s *Server) appLinkFor(container string) *state.AppLink {
	var out *state.AppLink
	s.store.Read(func(st *state.State) {
		for _, a := range st.Apps {
			if a.Container == container {
				a := a
				out = &a
				return
			}
		}
	})
	return out
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	cs, err := s.docker.Containers(r.Context())
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	byName := map[string]docker.Container{}
	for _, c := range cs {
		byName[firstName(c)] = c
	}
	var saved []state.AppLink
	var everSaved bool
	s.store.Read(func(st *state.State) {
		saved = append(saved, st.Apps...)
		everSaved = st.AppsSaved
	})
	apps := []appView{}
	linked := map[string]bool{}
	for _, a := range saved {
		v := appView{AppLink: a}
		if c, ok := byName[a.Container]; ok {
			v.State = c.State
		} else if a.Container != "" {
			v.State = "missing"
		}
		linked[a.Container] = true
		apps = append(apps, v)
	}
	suggestions := []appSuggestion{}
	for _, c := range cs {
		name := firstName(c)
		if linked[name] || name == "homestack" || c.Labels[docker.LabelProject] == "homestack" {
			continue
		}
		if u := webURL(c); u != "" {
			suggestions = append(suggestions, appSuggestion{Name: prettyName(c), URL: u, Container: name, Image: c.Image, State: c.State})
		}
	}
	sort.Slice(suggestions, func(i, j int) bool {
		return strings.ToLower(suggestions[i].Name) < strings.ToLower(suggestions[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps, "suggestions": suggestions, "saved": everSaved})
}

func (s *Server) putApps(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Apps []state.AppLink `json:"apps"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Apps) > 200 {
		writeErr(w, http.StatusBadRequest, "That's more links than the dashboard can show.")
		return
	}
	seen := map[string]bool{}
	for i := range req.Apps {
		a := &req.Apps[i]
		a.Name = strings.TrimSpace(a.Name)
		a.URL = strings.TrimSpace(a.URL)
		a.Container = strings.TrimSpace(a.Container)
		if a.Name == "" || len(a.Name) > 60 {
			writeErr(w, http.StatusBadRequest, "Give each link a name (up to 60 characters).")
			return
		}
		if err := validAppURL(a.URL); err != nil {
			writeErr(w, http.StatusBadRequest, a.Name+": "+err.Error())
			return
		}
		if len(a.Container) > 200 {
			writeErr(w, http.StatusBadRequest, "Container name too long.")
			return
		}
		if a.ID == "" || seen[a.ID] || len(a.ID) > 40 {
			a.ID = newAppID()
		}
		seen[a.ID] = true
	}
	err := s.store.Update(func(st *state.State) error {
		st.Apps = req.Apps
		st.AppsSaved = true
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save the links.")
		return
	}
	s.icons.forgetAll()
	s.listApps(w, r)
}

// setContainerWebUI saves (or clears) the web address for one container,
// from the container page.
func (s *Server) setContainerWebUI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Container string `json:"container"`
		Name      string `json:"name"`
		URL       string `json:"url"` // "" removes the link
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.Container == "" {
		writeErr(w, http.StatusBadRequest, "Missing container.")
		return
	}
	if req.URL != "" {
		if err := validAppURL(req.URL); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = req.Container
	}
	err := s.store.Update(func(st *state.State) error {
		for i, a := range st.Apps {
			if a.Container == req.Container {
				if req.URL == "" {
					st.Apps = append(st.Apps[:i], st.Apps[i+1:]...)
				} else {
					st.Apps[i].URL = req.URL
				}
				st.AppsSaved = true
				return nil
			}
		}
		if req.URL != "" {
			st.Apps = append(st.Apps, state.AppLink{ID: newAppID(), Name: strings.TrimSpace(req.Name), URL: req.URL, Container: req.Container})
			st.AppsSaved = true
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save the link.")
		return
	}
	s.icons.forgetAll()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- Icons ----------

// Icons come from each app's own web page (its favicon), fetched by the
// server over the LAN and cached, so the browser never loads anything from
// outside and the Content Security Policy stays strict.

type iconEntry struct {
	data    []byte
	ctype   string
	fetched time.Time
}

type iconCache struct {
	mu      sync.Mutex
	entries map[string]iconEntry // resolved URL -> icon (nil data = none found)
}

func newIconCache() *iconCache { return &iconCache{entries: map[string]iconEntry{}} }

func (ic *iconCache) forgetAll() {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	for k, e := range ic.entries {
		if e.data == nil {
			delete(ic.entries, k) // retry misses after the links change
		}
	}
}

// localOnly refuses connections to anything but loopback and private
// addresses, checked on the actual IP dialled (so DNS tricks can't point it
// at the internet).
func localOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return fmt.Errorf("icons are only fetched from the local network")
	}
	return nil
}

var iconClient = &http.Client{
	Timeout: 4 * time.Second,
	Transport: &http.Transport{
		Proxy:           nil,
		DialContext:     (&net.Dialer{Timeout: 3 * time.Second, Control: localOnly}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // home-lab apps often use self-signed certificates
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

var iconLink = regexp.MustCompile(`(?is)<link\b[^>]*>`)
var attr = func(tag, name string) string {
	m := regexp.MustCompile(`(?is)\b` + name + `\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`).FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return m[2] + m[3] + m[4]
}

func fetchLimited(ctx context.Context, u string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Homestack icon fetcher")
	resp, err := iconClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(b)) > limit {
		return nil, "", errors.New("too large")
	}
	return b, resp.Header.Get("Content-Type"), nil
}

var iconTypes = map[string]bool{"image/png": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
	"image/jpeg": true, "image/gif": true, "image/webp": true, "image/svg+xml": true}

func sniffIcon(b []byte, declared string) string {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	detected := http.DetectContentType(b)
	switch {
	case strings.HasPrefix(detected, "image/"):
		return detected
	case len(b) > 4 && b[0] == 0 && b[1] == 0 && b[2] == 1 && b[3] == 0:
		return "image/x-icon"
	case ct == "image/svg+xml" && strings.Contains(strings.ToLower(string(b[:min(len(b), 512)])), "<svg"):
		return "image/svg+xml"
	}
	return ""
}

func findIcon(ctx context.Context, page string) ([]byte, string) {
	base, err := url.Parse(page)
	if err != nil {
		return nil, ""
	}
	var candidates []string
	if html, _, err := fetchLimited(ctx, page, 512<<10); err == nil {
		type cand struct {
			href  string
			score int
		}
		var cs []cand
		for _, tag := range iconLink.FindAllString(string(html), 50) {
			rel := strings.ToLower(attr(tag, "rel"))
			href := attr(tag, "href")
			if href == "" || !strings.Contains(rel, "icon") || strings.Contains(rel, "mask") {
				continue
			}
			score := 1
			if strings.Contains(rel, "apple-touch-icon") {
				score = 3 // usually the biggest, sharpest one
			} else if strings.Contains(strings.ToLower(href), ".svg") || strings.Contains(strings.ToLower(href), ".png") {
				score = 2
			}
			cs = append(cs, cand{href, score})
		}
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
		for _, c := range cs {
			if ref, err := url.Parse(c.href); err == nil {
				candidates = append(candidates, base.ResolveReference(ref).String())
			}
		}
	}
	candidates = append(candidates, base.ResolveReference(&url.URL{Path: "/favicon.ico"}).String())
	for _, c := range candidates {
		if !strings.HasPrefix(c, "http://") && !strings.HasPrefix(c, "https://") {
			continue
		}
		b, ct, err := fetchLimited(ctx, c, 256<<10)
		if err != nil || len(b) == 0 {
			continue
		}
		if t := sniffIcon(b, ct); iconTypes[t] {
			return b, t
		}
	}
	return nil, ""
}

// appIcon serves the icon for one saved link.
func (s *Server) appIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var link *state.AppLink
	s.store.Read(func(st *state.State) {
		for _, a := range st.Apps {
			if a.ID == id {
				a := a
				link = &a
			}
		}
	})
	if link == nil {
		http.NotFound(w, r)
		return
	}
	// Homestack runs on the host's network, so [HOST] is this machine.
	resolved := strings.ReplaceAll(link.URL, hostToken, "127.0.0.1")
	s.icons.mu.Lock()
	e, ok := s.icons.entries[resolved]
	s.icons.mu.Unlock()
	fresh := ok && (time.Since(e.fetched) < 24*time.Hour && e.data != nil || time.Since(e.fetched) < 30*time.Minute)
	if !fresh {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		data, ctype := findIcon(ctx, resolved)
		cancel()
		e = iconEntry{data: data, ctype: ctype, fetched: time.Now()}
		s.icons.mu.Lock()
		s.icons.entries[resolved] = e
		s.icons.mu.Unlock()
	}
	if e.data == nil {
		w.Header().Set("Cache-Control", "private, max-age=600")
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", e.ctype)
	h.Set("Cache-Control", "private, max-age=86400")
	// An SVG opened directly must not be able to run scripts on Homestack's origin.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	_, _ = w.Write(e.data)
}
