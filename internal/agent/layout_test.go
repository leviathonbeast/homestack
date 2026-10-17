package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"homestack/internal/backup"
)

// srvLayout is a layout with nothing where Unraid puts it.
func srvLayout() Layout {
	return Layout{Shares: "/srv/shares", Array: "/srv/array", Cache: "/srv/fast", Appdata: "/srv/shares/apps",
		DiskPrefix: "/srv/data", ParityPrefix: "/srv/par", Unassigned: "/srv/usb", SnapRAID: "/etc/snapraid/snapraid.conf", Owner: "1000:1001"}
}

func TestLayoutValidate(t *testing.T) {
	for _, l := range []Layout{DefaultLayout(), srvLayout()} {
		if err := l.validate(); err != nil {
			t.Fatalf("%+v: %v", l, err)
		}
	}
	l := srvLayout()
	l.Owner, l.Appdata = " 0:0 ", ""
	if err := l.validate(); err != nil || l.Owner != "0:0" {
		t.Fatalf("tidied: %q %v", l.Owner, err)
	}
	bad := map[string]func(*Layout){
		"relative":          func(l *Layout) { l.Shares = "mnt/user" },
		"root":              func(l *Layout) { l.Cache = "/" },
		"system folder":     func(l *Layout) { l.Shares = "/var/user" },
		"dots":              func(l *Layout) { l.Cache = "/mnt/../cache" },
		"space":             func(l *Layout) { l.Cache = "/mnt/my cache" },
		"quote":             func(l *Layout) { l.Array = "/mnt/it's" },
		"same place":        func(l *Layout) { l.Array = "/mnt/user" },
		"inside another":    func(l *Layout) { l.Cache = "/mnt/user/cache" },
		"a disk's place":    func(l *Layout) { l.Cache = "/mnt/disk7" },
		"a parity's place":  func(l *Layout) { l.Unassigned = "/mnt/parity2" },
		"disks in shares":   func(l *Layout) { l.DiskPrefix = "/mnt/user/disk" },
		"prefix digit":      func(l *Layout) { l.DiskPrefix = "/mnt/disk0" },
		"prefixes overlap":  func(l *Layout) { l.ParityPrefix = "/mnt/disky" },
		"reserved pool":     func(l *Layout) { l.DiskPrefix = "/mnt/mirror" },
		"c pools":           func(l *Layout) { l.DiskPrefix = "/mnt/c" },
		"appdata elsewhere": func(l *Layout) { l.Appdata = "/mnt/cache/appdata" },
		"owner names":       func(l *Layout) { l.Owner = "nobody:users" },
		"owner too big":     func(l *Layout) { l.Owner = "99:70000" },
		"snapraid relative": func(l *Layout) { l.SnapRAID = "snapraid.conf" },
	}
	for name, f := range bad {
		l := DefaultLayout()
		f(&l)
		if l.validate() == nil {
			t.Errorf("%s: allowed %+v", name, l)
		}
	}
}

func TestLayoutPaths(t *testing.T) {
	l := DefaultLayout()
	for p, want := range map[string]int{"/mnt/disk3": 3, "/mnt/disk12/": 12, "/mnt/disks": 0, "/mnt/disk03": 0, "/mnt/disk": 0, "/mnt/disk3x": 0, "/mnt/disk+3": 0} {
		if got := l.DiskNum(p); got != want {
			t.Errorf("DiskNum(%s) = %d", p, got)
		}
	}
	if l.ParityNum("/mnt/parity2") != 2 || l.DiskPool(6) != "disk6" || l.DiskPath(6) != "/mnt/disk6" || l.ParityPath(3) != "/mnt/parity3" {
		t.Error("names")
	}
	if l.shareFolder("/mnt/user/Media/") != "Media" || l.shareFolder("/mnt/user") != "" || l.shareFolder("/mnt/cache/Media") != "" || l.shareFolder("/mnt/user/a/b") != "" {
		t.Error("shareFolder")
	}
	for p, want := range map[string]bool{"/mnt/user/Media": true, "/mnt/disk12/x": true, "/mnt/disks/usb/x": true, "/mnt/cache/x": true,
		"/mnt/disk3": false, "/mnt/diskx/a": false, "/etc/x": false, "/mnt/other/x": false, "/mnt/user-array/x": false} {
		if l.inStorage(p) != want {
			t.Errorf("inStorage(%s)", p)
		}
	}
	if l.where("/mnt/cache/backups") != "the cache" || l.where("/mnt/disk3/backups") != "disk3" || l.where("/mnt/user/backups") != "" {
		t.Error("where")
	}
	// A restore brings back the storage's fstab lines: with Unraid's paths,
	// everything under /mnt, as before.
	for p, want := range map[string]bool{"/mnt/disk3": true, "/mnt/user": true, "/mnt/disks/usb": true, "/mnt/backup-usb": true, "/": false, "/home": false, "/boot/efi": false} {
		if l.ownsMount(p) != want {
			t.Errorf("ownsMount(%s)", p)
		}
	}
	if !slices.Equal(l.arrayMounts(), []string{"/mnt/user-array", "/mnt/user", "/mnt/user/appdata"}) {
		t.Error("arrayMounts")
	}
	if u, g := l.OwnerIDs(); u != 99 || g != 100 {
		t.Error("owner")
	}

	s := srvLayout()
	s.Appdata = ""
	if s.DiskPool(2) != "data2" || s.DiskPath(2) != "/srv/data2" || !s.ownsMount("/srv/data2") || !s.ownsMount("/srv/anything") || s.ownsMount("/opt/x") || s.ownsMount("/mnt/user") {
		t.Error("custom paths")
	}
	if !s.inStorage("/srv/data2/Media") || s.inStorage("/mnt/user/Media") || s.where("/srv/data2/x") != "data2" {
		t.Error("custom storage")
	}
	if !slices.Equal(s.arrayMounts(), []string{"/srv/array", "/srv/shares"}) {
		t.Error("no apps' data mount")
	}
	if u, g := s.OwnerIDs(); u != 1000 || g != 1001 {
		t.Error("custom owner")
	}
}

// A new layout has to match the server: the views mounted (and mergerfs),
// the disks in snapraid.conf where it says, and no disk job running.
func TestCheckLayout(t *testing.T) {
	a, r, _ := assignHost(t)
	ctx := context.Background()
	old := a.layout()
	refused := func(l Layout, want string) {
		t.Helper()
		if err := a.checkLayout(ctx, old, l); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	if err := a.checkLayout(ctx, old, old); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
	l := old
	l.Shares, l.Appdata = "/srv/user", "/srv/user/appdata"
	refused(l, "nothing is mounted at /srv/user")
	r.out["findmnt -no FSTYPE --mountpoint /srv/user"] = "ext4\n"
	refused(l, "not a mergerfs view")
	r.out["findmnt -no FSTYPE --mountpoint /srv/user"] = "fuse.mergerfs\n"
	r.out["findmnt -no FSTYPE --mountpoint /srv/user/appdata"] = "!" // the fake matches prefixes
	refused(l, "nothing is mounted at /srv/user/appdata")
	r.out["findmnt -no FSTYPE --mountpoint /srv/user/appdata"] = "btrfs\n"
	if err := a.checkLayout(ctx, old, l); err != nil {
		t.Fatalf("mounted: %v", err)
	}

	l = old
	l.Cache = "/srv/cache"
	refused(l, "The cache: nothing is mounted")
	l = old
	l.DiskPrefix = "/srv/disk"
	refused(l, "snapraid.conf has d1 at /mnt/disk1, which isn't /srv/diskN")
	l = old
	l.ParityPrefix = "/srv/parity"
	refused(l, "on /mnt/parity1, which isn't /srv/parityN")
	l = old
	l.SnapRAID = "/etc/nothing.conf"
	refused(l, "couldn't read /etc/nothing.conf")
	l = old
	l.Owner = "1000:100" // nothing to check on the server
	if err := a.checkLayout(ctx, old, l); err != nil {
		t.Fatal(err)
	}

	a.saveOp(&ReplaceOp{Pool: "disk3", State: "running", Steps: []ReplaceStep{{Key: "fix", State: "running"}}})
	refused(l, "A disk job is in progress")
	a.saveOp(&ReplaceOp{Pool: "disk3", State: "done"})
	if err := a.checkLayout(ctx, old, l); err != nil {
		t.Fatalf("after the job: %v", err)
	}
}

// Saved for the agent (read again on start) and for the shell jobs.
func TestSetLayout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOMESTACK_ALERTS_CONF", filepath.Join(dir, "homestack-alerts"))
	a := New(Options{StateDir: dir, LogDir: dir})
	if a.layout() != DefaultLayout() {
		t.Fatal("default")
	}
	l := srvLayout()
	if err := a.setLayout(l); err != nil {
		t.Fatal(err)
	}
	if New(Options{StateDir: dir, LogDir: dir}).layout() != l {
		t.Error("not read back")
	}
	sh := readText(filepath.Join(dir, "homestack-layout"))
	for _, want := range []string{"STORAGE_SHARES='/srv/shares'\n", "STORAGE_ARRAY='/srv/array'\n", "STORAGE_CACHE='/srv/fast'\n",
		"STORAGE_DISK_PREFIX='/srv/data'\n", "STORAGE_PARITY_PREFIX='/srv/par'\n", "STORAGE_SNAPRAID='/etc/snapraid/snapraid.conf'\n", "STORAGE_OWNER='1000:1001'\n"} {
		if !strings.Contains(sh, want) {
			t.Errorf("shell file lacks %q:\n%s", want, sh)
		}
	}
	// A broken file isn't half-used: the defaults stand.
	os.WriteFile(filepath.Join(dir, "layout.json"), []byte(`{"shares":"relative"}`), 0o644)
	if New(Options{StateDir: dir, LogDir: dir}).layout() != DefaultLayout() {
		t.Error("broken file used")
	}
}

// The next data disk follows the layout's names.
func TestNextDataFollowsLayout(t *testing.T) {
	a, r, _ := assignHost(t)
	conf := r.path("etc/snapraid.conf")
	os.WriteFile(conf, []byte("parity /srv/par1/snapraid.parity\ndata d1 /srv/data1/\ndata d2 /srv/data2\n"), 0o644)
	c, err := readSnapConf(conf)
	if err != nil {
		t.Fatal(err)
	}
	if s := a.nextData(context.Background(), &assignPlan{conf: c}, nil); !strings.Contains(s.Blocked, "aren't mounted at /mnt/diskN") {
		t.Errorf("default layout: %+v", s)
	}
	if err := a.setLayout(srvLayout()); err != nil {
		t.Fatal(err)
	}
	if s := a.nextData(context.Background(), &assignPlan{conf: c}, nil); s.Path != "/srv/data3" || s.Pool != "data3" || s.Name != "d3" {
		t.Errorf("custom layout: %+v", s)
	}
}

// A backup carries the layout; a restore puts it back first, and brings
// back the fstab lines for its paths, not anyone else's.
func TestRestoreLayout(t *testing.T) {
	old, or := backupHost(t)
	if err := old.setLayout(srvLayout()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(or.path("etc/fstab"), []byte(testFstab+"UUID=fast /srv/fast btrfs defaults,nofail 0 0\n/srv/data1:/srv/data2 /srv/array fuse.mergerfs defaults,nofail 0 0\nUUID=x /opt/thing ext4 defaults 0 0\n"), 0o644)
	k, _ := backup.NewKey(testPass)
	old.saveBackupSettings(backupSettings{Dir: "/srv/shares/backups", Keep: 14, Key: &k})
	or.out["findmnt -rno SOURCE --target /srv/shares/backups"] = "shares\n"
	os.MkdirAll(or.path("srv/shares/backups"), 0o755)
	f, err := old.makeBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, files := openBackupFile(t, filepath.Join(old.sys("/srv/shares/backups"), f.Name))
	if _, ok := files["storage/layout.json"]; !ok {
		t.Fatal("no layout in the backup")
	}

	a, r, _ := assignHost(t)
	t.Setenv("HOMESTACK_ALERTS_CONF", r.path("etc/default/homestack-alerts"))
	os.WriteFile(r.path("etc/fstab"), []byte("UUID=new-root / ext4 errors=remount-ro 0 1\n"), 0o644)
	r.mounted = map[string]string{}
	for k, v := range map[string]string{"zpool import": "", "mount -a": ""} {
		r.out[k] = v
	}
	res := a.restoreBackup(context.Background(), files, []string{"storage"})
	if len(res) != 1 || !res[0].OK || !strings.Contains(res[0].Detail, "the storage layout") {
		t.Fatalf("%+v", res)
	}
	if a.layout() != srvLayout() || !strings.Contains(readText(r.path("etc/default/homestack-layout")), "STORAGE_CACHE='/srv/fast'") {
		t.Error("layout not restored")
	}
	fstab := readText(r.path("etc/fstab"))
	if !strings.Contains(fstab, "/srv/fast btrfs") || !strings.Contains(fstab, "/srv/array fuse.mergerfs") || strings.Contains(fstab, "/opt/thing") || strings.Contains(fstab, "/mnt/user") {
		t.Errorf("fstab:\n%s", fstab)
	}
}

// A transfer's root is the deepest folder holding both sides.
func TestCommonDir(t *testing.T) {
	for _, c := range []struct {
		want  string
		paths []string
	}{
		{"/mnt/user/Media", []string{"/mnt/user/Media", "/mnt/user/Media/a", "/mnt/user/Media/b/c"}},
		{"/mnt", []string{"/mnt/disk1/Media", "/mnt/cache/Media/x"}},
		{"/mnt", []string{"/mnt/disk1", "/mnt/disk10/x"}}, // disk1 isn't a folder of disk10
		{"/", []string{"/srv/fast/x", "/mnt/disk1/y"}},
	} {
		if got := commonDir(c.paths...); got != c.want {
			t.Errorf("%v: %s, want %s", c.paths, got, c.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/data/it's here"); got != `'/data/it'\''s here'` {
		t.Error(got)
	}
}
