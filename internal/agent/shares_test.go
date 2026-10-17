package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lucyConf = `[global]
   workgroup = WORKGROUP
   guest account = unraid

[Media]
   path = /mnt/user/Media
   browseable = yes
   valid users = andrew dune
   read only = yes
   write list = andrew

[downloads]
   path = /mnt/user/downloads
   writeable = yes
   guest ok = yes

[Hidden]
   path = /mnt/user/Hidden
   browseable = no
   guest ok = yes
   write list = andrew

[Scratch]
   path = /mnt/disks/242895D00061
   valid users = andrew
   read only = no
`

func TestImportShares(t *testing.T) {
	_, secs := splitConf(lucyConf)
	got := map[string]Share{}
	for _, s := range secs {
		if specialSections[strings.ToLower(s.name)] {
			continue
		}
		got[s.name] = shareFromParams(s.name, parseParams(s.lines), []string{"andrew", "dune"})
	}
	m := got["Media"]
	if m.Security != "private" || m.Access["andrew"] != "rw" || m.Access["dune"] != "ro" || m.Export != "yes" {
		t.Fatalf("Media %+v", m)
	}
	if d := got["downloads"]; d.Security != "public" {
		t.Fatalf("downloads %+v", d)
	}
	if h := got["Hidden"]; h.Security != "secure" || h.Export != "hidden" || h.Access["andrew"] != "rw" {
		t.Fatalf("Hidden %+v", h)
	}
	if s := got["Scratch"]; s.Security != "private" || s.Access["andrew"] != "rw" || s.Path != "/mnt/disks/242895D00061" {
		t.Fatalf("Scratch %+v", s)
	}
	// Round trip: rendering gives the same effective settings.
	out := renderShares([]Share{m, got["downloads"], got["Hidden"]})
	for _, want := range []string{"[Media]\n   path = /mnt/user/Media", "valid users = andrew dune\n   read only = yes\n   write list = andrew", "[downloads]", "guest ok = yes\n   read only = no", "[Hidden]", "browseable = no"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestShareValidate(t *testing.T) {
	users := map[string]bool{"andrew": true}
	bad := []Share{
		{Name: "global", Path: "/mnt/user/x", Export: "yes", Security: "public"},
		{Name: "ok", Path: "/etc", Export: "yes", Security: "public"},
		{Name: "ok", Path: "/mnt/user/../../etc", Export: "yes", Security: "public"},
		{Name: "bad]name", Path: "/mnt/user/x", Export: "yes", Security: "public"},
		{Name: "ok", Path: "/mnt/user/x", Export: "yes", Security: "open"},
	}
	for _, s := range bad {
		if s.validate(DefaultLayout(), users) == nil {
			t.Errorf("accepted %+v", s)
		}
	}
	s := Share{Name: "Films 4K", Path: "/mnt/user/Films 4K/", Export: "hidden", Security: "private", Access: map[string]string{"andrew": "rw", "ghost": "rw", "": "x"}}
	if err := s.validate(DefaultLayout(), users); err != nil || s.Path != "/mnt/user/Films 4K" || len(s.Access) != 1 {
		t.Fatalf("%v %+v", err, s)
	}
}

func TestResolveStaysInside(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"mnt/user/Media", "mnt/disks/ssd", "etc"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.Symlink(filepath.Join(root, "etc"), filepath.Join(root, "mnt/disks/ssd/escape"))
	os.Symlink(filepath.Join(root, "mnt/disks/ssd"), filepath.Join(root, "mnt/disks/inside"))
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), LogDir: t.TempDir()})
	if _, _, err := a.resolve(t.Context(), "/mnt/disks/inside", true); err != nil {
		t.Fatalf("symlink inside the roots refused: %v", err)
	}
	if _, _, err := a.resolve(t.Context(), "/mnt/disks/ssd", true); err != nil {
		t.Fatalf("disks root: %v", err)
	}
	for _, p := range []string{"/etc", "/mnt/user/../../etc", "/mnt/disks/ssd/escape", "/mnt/disks/ssd/escape/passwd", "relative", "/mnt/userx"} {
		if _, _, err := a.resolve(t.Context(), p, true); err == nil {
			t.Errorf("%s allowed", p)
		}
	}
}

func TestMoverShares(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "homestack-alerts")
	os.WriteFile(conf, []byte("# x\nMOVER_SHARES=\"Media Music Public downloads isos\"  # comment\nMOVER_AGE_MIN=1440\n"), 0o644)
	t.Setenv("HOMESTACK_ALERTS_CONF", conf)
	a := New(Options{StateDir: dir, LogDir: dir})
	if got := strings.Join(a.alertsMoverShares(), ","); got != "Media,Music,Public,downloads,isos" {
		t.Fatalf("got %q", got)
	}
	st := sharesState{Shares: []Share{
		{Name: "Media", Path: "/mnt/user/Media", Mover: true},
		{Name: "Music", Path: "/mnt/user/Music", Mover: false}, // switched off
		{Name: "Photos", Path: "/mnt/user/Photos", Mover: true},
		{Name: "Scratch", Path: "/mnt/disks/ssd", Mover: false},
	}}
	if err := a.writeMoverShares(st); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "homestack-mover-shares"))
	// Public, downloads and isos aren't shares here, so they stay as before.
	if !strings.Contains(string(b), `MOVER_SHARES="Media Photos Public downloads isos"`) {
		t.Fatalf("file:\n%s", b)
	}
	s := Share{Name: "x", Path: "/mnt/disks/ssd", Export: "yes", Security: "public", Mover: true}
	if s.validate(DefaultLayout(), map[string]bool{}) == nil {
		t.Fatal("mover allowed outside /mnt/user")
	}
	if DefaultLayout().shareFolder("/mnt/user/Films 4K") != "" || DefaultLayout().shareFolder("/mnt/user/a/b") != "" || DefaultLayout().shareFolder("/mnt/user/Media/") != "Media" {
		t.Fatal("shareFolder")
	}
}
