package server

import (
	"testing"

	"homestack/internal/docker"
)

func ctr(image, netmode string, labels map[string]string, ports ...docker.Port) docker.Container {
	c := docker.Container{ID: "0123456789abcdef", Names: []string{"/x"}, Image: image, Labels: labels, Ports: ports}
	c.HostConfig.NetworkMode = netmode
	return c
}

func TestWebURL(t *testing.T) {
	tcp := func(priv, pub int) docker.Port { return docker.Port{PrivatePort: priv, PublicPort: pub, Type: "tcp"} }
	cases := []struct {
		name string
		c    docker.Container
		want string
	}{
		{"sabnzbd published 8097->8080", ctr("binhex/arch-sabnzbd", "bridge", nil, tcp(8090, 8098), tcp(8080, 8097)), "http://[HOST]:8097/"},
		{"sonarr on host network", ctr("binhex/arch-sonarr:latest", "host", nil), "http://[HOST]:8989/"},
		{"plex path", ctr("plexinc/pms-docker", "host", nil), "http://[HOST]:32400/web"},
		{"unraid webui label", ctr("some/app", "bridge", map[string]string{"net.unraid.docker.webui": "http://[IP]:[PORT:7000]/ui"}, tcp(7000, 17000)), "http://[HOST]:17000/ui"},
		{"label port not published falls back", ctr("some/app", "bridge", map[string]string{"net.unraid.docker.webui": "http://[IP]:[PORT:7000]/"}, tcp(80, 8123)), "http://[HOST]:8123/"},
		{"database only", ctr("postgres:16", "bridge", nil, tcp(5432, 5432)), ""},
		{"nothing published", ctr("redis", "bridge", nil), ""},
		{"prefers web port", ctr("custom/thing", "bridge", nil, tcp(1883, 1883), tcp(9999, 9999), tcp(80, 8088)), "http://[HOST]:8088/"},
		{"https port", ctr("custom/thing", "bridge", nil, tcp(443, 8443)), "https://[HOST]:8443/"},
		{"udp ignored", ctr("custom/thing", "bridge", nil, docker.Port{PrivatePort: 8080, PublicPort: 8080, Type: "udp"}), ""},
	}
	for _, tc := range cases {
		if got := webURL(tc.c); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestValidAppURL(t *testing.T) {
	for _, ok := range []string{"http://[HOST]:8097/", "https://jellyfin.home.lan/", "http://10.0.10.5:8080/admin"} {
		if err := validAppURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "ftp://x/", "http://", "//host/", "http://user:pw@host/", "data:text/html,hi"} {
		if validAppURL(bad) == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}
