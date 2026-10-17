package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"homestack/internal/docker"
	"homestack/internal/state"
	"homestack/internal/templates"
)

func TestIPAllowed(t *testing.T) {
	nets := []state.Network{{CIDR: "192.168.1.0/24"}, {CIDR: "10.8.0.0/24"}}
	cases := map[string]bool{
		"192.168.1.20": true,
		"10.8.0.5":     true,
		"10.9.0.5":     false,
		"8.8.8.8":      false,
	}
	for ip, want := range cases {
		if got := ipAllowed(netip.MustParseAddr(ip), nets); got != want {
			t.Errorf("%s: got %v, want %v", ip, got, want)
		}
	}
}

func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.9:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := s.clientIP(r).String(); got != "192.168.1.9" {
		t.Fatalf("untrusted peer: got %s", got)
	}
	_, n, _ := parseCIDR("192.168.1.9/32")
	s.cfg.TrustedProxies = append(s.cfg.TrustedProxies, n)
	if got := s.clientIP(r).String(); got != "1.2.3.4" {
		t.Fatalf("trusted proxy: got %s", got)
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"nginx":                   "nginx:latest",
		"nginx:alpine":            "nginx:alpine",
		"registry.local:5000/app": "registry.local:5000/app:latest",
		"ghcr.io/owner/app:1.2":   "ghcr.io/owner/app:1.2",
		"nginx@sha256:abc":        "nginx@sha256:abc",
	}
	for in, want := range cases {
		if got := normalizeRef(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestSplitRef(t *testing.T) {
	img, tag := docker.SplitRef("registry.local:5000/app")
	if img != "registry.local:5000/app" || tag != "latest" {
		t.Fatalf("got %s %s", img, tag)
	}
	img, tag = docker.SplitRef("lscr.io/linuxserver/plex:1.40")
	if img != "lscr.io/linuxserver/plex" || tag != "1.40" {
		t.Fatalf("got %s %s", img, tag)
	}
}

func TestComposeProjectName(t *testing.T) {
	if got := composeProjectName("/opt/stacks/My.Media Server"); got != "mymediaserver" {
		t.Fatalf("got %s", got)
	}
}

func TestSinceArg(t *testing.T) {
	if got := sinceArg("2026-09-24T11:29:10.239916762Z"); got != "1790249350.239916763" {
		t.Fatalf("got %s", got)
	}
}

func TestStackDirRejectsTraversal(t *testing.T) {
	st, err := state.Open(t.TempDir()+"/s.json", "/opt/stacks")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	for _, bad := range []string{"..", "../etc", ".trash", "a/b", ""} {
		if _, err := s.stackDir(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if d, err := s.stackDir("jellyfin"); err != nil || d != "/opt/stacks/jellyfin" {
		t.Errorf("got %s %v", d, err)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := state.HashPassword("correct-horse-9")
	if err != nil {
		t.Fatal(err)
	}
	if !state.CheckPassword(h, "correct-horse-9") || state.CheckPassword(h, "wrong") {
		t.Fatal("password check failed")
	}
}

func parseCIDR(s string) (netip.Prefix, *net.IPNet, error) {
	p := netip.MustParsePrefix(s)
	_, n, err := net.ParseCIDR(s)
	return p, n, err
}

func TestValidLinkHost(t *testing.T) {
	for h, want := range map[string]bool{
		"10.0.10.5": true, "lucy.lan": true, "lucy": true, "2001:db8:0:8a00::5": true,
		"http://10.0.10.5": false, "10.0.10.5:8080": false, "lucy.lan/apps": false, "-bad.lan": false, "": false,
	} {
		if got := validLinkHost(h); got != want {
			t.Errorf("%q: got %v, want %v", h, got, want)
		}
	}
}

func TestCleanFolders(t *testing.T) {
	in := []state.ContainerFolder{
		{ID: "a", Name: " Media ", Icon: "film", Color: "#4f46e5", Containers: []string{"jellyfin", "/sonarr", "jellyfin", "bad name"}},
		{ID: "a", Name: "Downloads", Icon: "<svg>", Color: "red", Match: "^(qbit|sab)", Containers: []string{"sonarr", "qbittorrent"}},
	}
	out, err := cleanFolders(in)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Name != "Media" || strings.Join(out[0].Containers, ",") != "jellyfin,sonarr" {
		t.Errorf("first folder %+v", out[0])
	}
	if out[1].ID == "a" || out[1].Icon != "" || out[1].Color != "" || strings.Join(out[1].Containers, ",") != "qbittorrent" {
		t.Errorf("second folder %+v", out[1])
	}
	for _, bad := range [][]state.ContainerFolder{{{Name: ""}}, {{Name: "x", Match: "("}}} {
		if _, err := cleanFolders(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// Templates are filled in for this server: the browser's time zone, the
// address it reached Homestack on (or the app links setting), and the
// default storage layout when there's no agent.
func TestTemplateValues(t *testing.T) {
	t.Setenv("HOMESTACK_AGENT_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	st, err := state.Open(filepath.Join(t.TempDir(), "homestack.json"), "/opt/stacks")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	get := func(target, host string) templates.Values {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Host = host
		return s.templateValues(r)
	}
	if v := get("/api/templates?tz=America/Chicago", "10.1.2.3:5080"); v != (templates.Values{TZ: "America/Chicago", Host: "10.1.2.3", Shares: "/mnt/user", PUID: "99", PGID: "100"}) {
		t.Errorf("%+v", v)
	}
	if v := get("/api/templates?tz=../../etc/passwd", "[fd00::5]:5080"); v.TZ != "Etc/UTC" || v.Host != "[fd00::5]" {
		t.Errorf("%+v", v)
	}
	st.Update(func(x *state.State) error { x.Settings.AppHost = "apps.lan"; return nil })
	if v := get("/api/templates", "10.1.2.3:5080"); v.Host != "apps.lan" || v.TZ != "Etc/UTC" {
		t.Errorf("%+v", v)
	}
}
