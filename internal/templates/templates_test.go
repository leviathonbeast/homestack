package templates

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var reVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?-[^}]*)?\}`)

func TestTemplates(t *testing.T) {
	ids := map[string]bool{}
	for _, tp := range All() {
		if tp.ID == "" || ids[tp.ID] || tp.Name == "" || tp.Category == "" || tp.Description == "" {
			t.Errorf("%q: missing or duplicate fields", tp.ID)
		}
		ids[tp.ID] = true
		if !strings.HasPrefix(tp.Compose, "services:\n") || strings.Contains(tp.Compose, "\t") {
			t.Errorf("%s: compose must start with services: and use spaces", tp.ID)
		}
		env := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(tp.Env), "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, _, ok := strings.Cut(line, "=")
			if !ok || k == "" || env[k] {
				t.Errorf("%s: bad or repeated .env line %q", tp.ID, line)
			}
			env[k] = true
		}
		for _, m := range reVar.FindAllStringSubmatch(tp.Compose, -1) {
			if !env[m[1]] && m[2] == "" {
				t.Errorf("%s: ${%s} isn't in its .env", tp.ID, m[1])
			}
		}
		// A comment always sits above each variable: it's the form's hint.
		lines := strings.Split(tp.Env, "\n")
		for i, line := range lines {
			if line != "" && !strings.HasPrefix(line, "#") && (i == 0 || !strings.HasPrefix(lines[i-1], "# ")) {
				t.Errorf("%s: %q has no hint above it", tp.ID, line)
			}
		}
	}
}

// Every placeholder is one With fills in.
func TestWith(t *testing.T) {
	v := Values{TZ: "America/New_York", Host: "192.168.1.20", Shares: "/srv/shares", PUID: "1000", PGID: "1001"}
	byID := map[string]Template{}
	for _, tp := range With(v) {
		if strings.Contains(tp.Env, "{{") || strings.Contains(tp.Notes, "{{") {
			t.Errorf("%s: a placeholder is left:\n%s", tp.ID, tp.Env)
		}
		byID[tp.ID] = tp
	}
	for id, want := range map[string]string{
		"jellyfin": "MEDIA_PATH=/srv/shares/Media\n", "arr": "PUID=1000\n# Group ID the app runs as (the shares' group)\nPGID=1001\n", "mealie": "BASE_URL=http://192.168.1.20:9925\n",
		"homepage": "ALLOWED_HOSTS=192.168.1.20:3002\n", "ntfy": "TZ=America/New_York\n",
	} {
		if !strings.Contains(byID[id].Env, want) {
			t.Errorf("%s: no %q in\n%s", id, want, byID[id].Env)
		}
	}
}

// TestWriteTemplates writes every template to $HOMESTACK_TEMPLATES_OUT, so
// they can be checked with `docker compose config`.
func TestWriteTemplates(t *testing.T) {
	out := os.Getenv("HOMESTACK_TEMPLATES_OUT")
	if out == "" {
		t.Skip("set HOMESTACK_TEMPLATES_OUT to write the templates out")
	}
	for _, tp := range With(Values{TZ: "Europe/London", Host: "192.168.1.20", Shares: "/mnt/user", PUID: "99", PGID: "100"}) {
		dir := filepath.Join(out, tp.ID)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(tp.Compose), 0o644)
		_ = os.WriteFile(filepath.Join(dir, ".env"), []byte(tp.Env), 0o644)
	}
}
