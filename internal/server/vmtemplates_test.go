package server

import (
	"testing"

	"homestack/internal/libvirt"
	"homestack/internal/state"
)

func templateServer(t *testing.T) *Server {
	t.Helper()
	st, err := state.Open(t.TempDir()+"/s.json", "/opt/stacks")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Update(func(s *state.State) error {
		s.VMTemplates = []state.VMTemplate{{ID: "debian-13", Name: "Debian 13", Path: "/srv/old-templates/debian-13.qcow2"}}
		return nil
	})
	return &Server{store: st}
}

func TestTemplateDiskGuard(t *testing.T) {
	s := templateServer(t)
	if got := s.store.Settings().VMTemplatesDir; got != "/mnt/user/domains/templates" {
		t.Fatalf("default templates folder %q", got)
	}
	for _, bad := range []string{
		"/mnt/user/domains/templates/debian-13.qcow2",
		"/mnt/user/domains/templates/../templates/x.qcow2",
		"/srv/old-templates/debian-13.qcow2", // registered, outside today's folder
	} {
		if err := s.templateDiskGuard("/mnt/user/domains/web/vdisk1.qcow2", bad); err == nil {
			t.Errorf("%s: a template's image must not become a VM disk", bad)
		}
	}
	if err := s.templateDiskGuard("/mnt/user/domains/web/vdisk1.qcow2", "/mnt/user/domains/templates-old/x.qcow2", "", "relative.qcow2"); err != nil {
		t.Errorf("ordinary disks refused: %v", err)
	}
}

func TestVMNameGuard(t *testing.T) {
	s := templateServer(t)
	for _, bad := range []string{"templates", "New", "options"} {
		if err := s.vmNameGuard(bad, ""); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	_ = s.store.Update(func(st *state.State) error { st.Settings.VMTemplatesDir = "/mnt/user/domains/golden"; return nil })
	if err := s.vmNameGuard("golden", "/mnt/user/domains"); err == nil {
		t.Error("a VM folder can't be the templates folder")
	}
	if err := s.vmNameGuard("web01", ""); err != nil {
		t.Error(err)
	}
}

func TestClaimTemplate(t *testing.T) {
	s := templateServer(t)
	a, err := s.claimTemplate("Debian 13", "https://example.com/a.qcow2")
	if err != nil || a.get().ID != "debian-13-2" {
		t.Fatalf("got %v %v (the id must not clash with an existing template)", a, err)
	}
	if _, err := s.claimTemplate("Other name", "https://example.com/a.qcow2"); err == nil {
		t.Fatal("the same download twice at once must be refused")
	}
	b, err := s.claimTemplate("Debian 13", "https://example.com/b.qcow2")
	if err != nil || b.get().ID != "debian-13-3" {
		t.Fatalf("got %v %v", b.get().ID, err)
	}
	s.releaseTemplate(a.get().ID)
	if _, err := s.claimTemplate("x", "https://example.com/a.qcow2"); err != nil {
		t.Fatal("released work can run again")
	}
	if c, _ := s.claimTemplate("../../etc/passwd!", "k"); c.get().ID != "etc-passwd" {
		t.Fatalf("id %q isn't file-safe", c.get().ID)
	}
}

func TestCheckImage(t *testing.T) {
	cases := []struct {
		info libvirt.VolInfo
		kind string
		ok   bool
	}{
		{libvirt.VolInfo{Format: "qcow2"}, "qcow2", true},
		{libvirt.VolInfo{Format: "raw"}, "raw", true},
		{libvirt.VolInfo{Format: "qcow2", Backing: "/etc/shadow"}, "qcow2", false},
		{libvirt.VolInfo{Format: "raw"}, "qcow2", false},
		{libvirt.VolInfo{Format: "vmdk"}, "", false},
	}
	for _, c := range cases {
		if err := checkImage(c.info, c.kind); (err == nil) != c.ok {
			t.Errorf("%+v %s: err %v", c.info, c.kind, err)
		}
	}
}
