package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// hostRunner is a fake server for the assign steps: it keeps track of
// what's mounted (mount <path> reads the fake /etc/fstab), and answers
// other commands from out, by the longest matching prefix.
type hostRunner struct {
	root      string
	ran       []string
	mounted   map[string]string // mount point -> filesystem UUID
	out       map[string]string // "!" first: the command fails
	failMount map[string]bool   // "target:uuid" that won't mount
}

func (r *hostRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.ran = append(r.ran, line)
	switch {
	case name == "findmnt" && len(args) == 4 && args[1] == "UUID" && args[2] == "--mountpoint":
		if u, ok := r.mounted[args[3]]; ok {
			return []byte(u + "\n"), nil
		}
		return nil, errors.New("exit 1")
	case name == "findmnt" && len(args) == 2 && args[0] == "--mountpoint":
		if _, ok := r.mounted[args[1]]; ok {
			return []byte("TARGET SOURCE\n"), nil
		}
		return nil, errors.New("exit 1")
	case name == "findmnt" && slices.Contains(args, "--target"):
		// Like the real one: a path that isn't there is an error.
		if _, err := os.Stat(filepath.Join(r.root, args[len(args)-1])); err != nil {
			return []byte("findmnt: " + args[len(args)-1] + ": No such file or directory\n"), errors.New("exit 1")
		}
	case name == "mount":
		target := args[len(args)-1]
		uuid := ""
		if len(args) == 1 { // from /etc/fstab
			if f := fstabEntry(readText(filepath.Join(r.root, "etc/fstab")), target); f != nil {
				uuid = strings.TrimPrefix(f[0], "UUID=")
			}
		} else {
			uuid = strings.TrimPrefix(args[len(args)-2], "UUID=")
		}
		if uuid == "" || r.failMount[target+":"+uuid] {
			return []byte("mount: wrong fs type, bad option, bad superblock"), errors.New("exit 32")
		}
		r.mounted[target] = uuid
		return nil, nil
	case name == "umount":
		delete(r.mounted, args[0])
		return nil, nil
	}
	best := ""
	for k := range r.out {
		if strings.HasPrefix(line, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return nil, errors.New("exit 1")
	}
	if v := r.out[best]; strings.HasPrefix(v, "!") {
		return []byte(v[1:]), errors.New("exit 1")
	}
	return []byte(r.out[best]), nil
}

const testFstab = `# /etc/fstab
UUID=root-uuid  /  ext4  errors=remount-ro  0  1
UUID=old-uuid  /mnt/parity1  ext4  defaults,nofail  0  2
/mnt/disk1:/mnt/disk2  /mnt/user-array  fuse.mergerfs  cache.files=off,category.create=epmfs,minfreespace=50G  0 0
/mnt/cache:/mnt/disk1:/mnt/disk2  /mnt/user  fuse.mergerfs  cache.files=off,category.create=ff,minfreespace=100G  0 0
`

const testSnapConf = `# SnapRAID
parity /mnt/parity1/snapraid.parity

content /var/snapraid/snapraid.content
content /mnt/disk1/snapraid.content

data d1 /mnt/disk1/
data d2 /mnt/disk2/

exclude *.tmp

# SnapRAID 13 to 15 options Homestack doesn't use itself: kept as they are.
autosave 500
temp_limit 50
snapshot
`

// assignHost is a server with two data disks, one parity disk, mergerfs
// views of them, and a spare disk sdf (serial NEW1).
func assignHost(t *testing.T) (*Agent, *hostRunner, map[string]string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"etc", "dev/disk/by-id", "mnt/disk1/Media", "mnt/disk1/Music", "mnt/disk2/Media", "mnt/disk2/Backups",
		"mnt/disk2/.Trash", "mnt/parity1", "mnt/user", "mnt/user-array", "mnt/cache"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "etc/fstab"), []byte(testFstab), 0o644)
	conf := filepath.Join(root, "etc/snapraid.conf")
	os.WriteFile(conf, []byte(testSnapConf), 0o644)
	t.Setenv("HOMESTACK_SNAPRAID_CONF", conf)
	os.WriteFile(filepath.Join(root, "dev/sdf"), nil, 0o644)
	os.Symlink("../../sdf", filepath.Join(root, "dev/disk/by-id/ata-NEW_DISK_NEW1"))
	parity := make([]byte, 3<<20)
	rand.Read(parity)
	os.WriteFile(filepath.Join(root, "mnt/parity1/snapraid.parity"), parity, 0o600)
	os.WriteFile(filepath.Join(root, "mnt/parity1/snapraid.content"), []byte("content"), 0o600)

	r := &hostRunner{root: root, mounted: map[string]string{"/mnt/parity1": "old-uuid", "/mnt/disk1": "", "/mnt/disk2": ""},
		failMount: map[string]bool{}, out: map[string]string{
			"findmnt -rn -t fuse.mergerfs -o TARGET": "/mnt/user\n/mnt/user-array\n/mnt/backups\n",
			"findmnt --verify --tab-file":            "Success, no errors or warnings detected\n",
			"systemctl daemon-reload":                "",
			"which":                                  "/usr/bin/x\n",
		}}
	branches := map[string]string{
		filepath.Join(root, "mnt/user/.mergerfs"):       "/mnt/cache=RW:/mnt/disk1=RW:/mnt/disk2=RW",
		filepath.Join(root, "mnt/user-array/.mergerfs"): "/mnt/disk1=RW:/mnt/disk2=RW",
		filepath.Join(root, "mnt/backups/.mergerfs"):    "/mnt/usb1=RW:/mnt/usb2=RW", // not the data disks: left alone
	}
	// The array lock and the settings files in the test's own folders, so it
	// runs without root (as on GitHub's runners).
	t.Setenv("HOMESTACK_ALERTS_CONF", filepath.Join(root, "etc/default/homestack-alerts"))
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), Runner: r, LockFile: filepath.Join(t.TempDir(), "array.lock")})
	a.xattr = xattrOps{
		get: func(p, _ string) (string, error) {
			if v, ok := branches[p]; ok {
				return v, nil
			}
			return "", errors.New("no such attribute")
		},
		set: func(p, _, v string) error {
			switch {
			case strings.HasPrefix(v, "+>"):
				branches[p] += ":" + v[2:] + "=RW"
			case strings.HasPrefix(v, "-"):
				var keep []string
				for b := range strings.SplitSeq(branches[p], ":") {
					if strings.SplitN(b, "=", 2)[0] != v[1:] {
						keep = append(keep, b)
					}
				}
				branches[p] = strings.Join(keep, ":")
			default:
				return errors.New("unexpected value " + v)
			}
			return nil
		},
	}
	a.disks.disks = []Disk{{Name: "sdf", Serial: "NEW1", Model: "NEW DISK", Size: 16e12}}
	return a, r, branches
}

func (r *hostRunner) path(p string) string { return filepath.Join(r.root, p) }

func TestAssignBlocked(t *testing.T) {
	ok := []ArraySlot{{Pool: "disk1", State: "ONLINE", Mounted: true}, {Pool: "disk2", State: "ONLINE", Mounted: true}}
	par := []ParitySlot{{Label: "Parity 1", Mount: "/mnt/parity1", Mounted: true}}
	if b := assignBlocked(nil, ok, par, nil); b != "" {
		t.Fatalf("healthy array blocked: %s", b)
	}
	cases := map[string]string{
		"a job running":  assignBlocked(&ReplaceOp{Method: "rebuild", Pool: "disk3", State: "paused"}, ok, par, nil),
		"a missing disk": assignBlocked(nil, append(slices.Clone(ok), ArraySlot{Pool: "disk3", State: "MISSING"}), par, nil),
		"failing SMART":  assignBlocked(nil, []ArraySlot{{Pool: "disk1", State: "ONLINE", Mounted: true, SmartFailing: true}}, par, nil),
		"resilvering":    assignBlocked(nil, ok, par, []Pool{{Name: "disk2", ScanState: "resilvering"}}),
	}
	for name, b := range cases {
		if b == "" {
			t.Errorf("%s: not blocked", name)
		}
	}
	if !strings.Contains(cases["a job running"], "Replacing disk3 is in progress") || !strings.Contains(cases["a missing disk"], "disk3 isn't working (missing)") {
		t.Errorf("reasons: %q", cases)
	}
	// A finished job doesn't block.
	if b := assignBlocked(&ReplaceOp{State: "done"}, ok, par, nil); b != "" {
		t.Errorf("done job blocked: %s", b)
	}
	// Nor does a parity disk that's down: replacing it is how it comes back.
	if b := assignBlocked(nil, ok, []ParitySlot{{Label: "Parity 1", Mount: "/mnt/parity1"}}, nil); b != "" {
		t.Errorf("parity down blocked everything: %s", b)
	}
}

func TestParityCheck(t *testing.T) {
	up := ParitySlot{Name: "parity", Label: "Parity 1", Mounted: true, FSType: "ext4", UUID: "u1"}
	down := ParitySlot{Name: "2-parity", Label: "Parity 2", FSType: "xfs", Source: "UUID=u2"}
	all := []ParitySlot{up, down}
	cases := []struct {
		action string
		s      ParitySlot
		all    []ParitySlot
		want   string // "" = allowed
	}{
		{"replace-parity", up, []ParitySlot{up}, ""},
		{"rebuild-parity", down, []ParitySlot{down}, ""},
		{"replace-parity", down, []ParitySlot{down}, "has to be rebuilt instead"},
		{"rebuild-parity", up, []ParitySlot{up}, "mounted again"},
		{"replace-parity", up, all, "Parity 2 isn't mounted either"}, // one at a time
		{"rebuild-parity", ParitySlot{Name: "parity", Label: "Parity 1", FSType: "ext4"}, nil, "no line for"},
		{"rebuild-parity", ParitySlot{Name: "parity", Label: "Parity 1", FSType: "btrfs", Source: "UUID=x"}, nil, "only sets up ext4 or XFS"},
		{"replace-parity", ParitySlot{Name: "parity", Label: "Parity 1", Mounted: true, FSType: "ext4"}, nil, "no UUID"},
	}
	for _, c := range cases {
		err := parityCheck(c.action, &c.s, c.all)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s %s: %v, want %q", c.action, c.s.Label, err, c.want)
		}
	}
	if err := parityCheck("rebuild-parity", nil, nil); err == nil {
		t.Error("no slot accepted")
	}
}

func TestRebuildParitySteps(t *testing.T) {
	a, r, _ := assignHost(t)
	ctx := context.Background()
	// Parity 1's disk died: nothing is mounted at /mnt/parity1, and it
	// won't mount again.
	delete(r.mounted, "/mnt/parity1")
	r.failMount["/mnt/parity1:old-uuid"] = true
	op := &ReplaceOp{ID: "job5", Method: "rebuild-parity", Snap: "parity", Pool: "parity 1", Path: "/mnt/parity1",
		ParityFile: "/mnt/parity1/snapraid.parity", FSType: "ext4", OldUUID: "old-uuid", NewUUID: "new-uuid"}
	op.Steps = assignSteps(op)
	if keys := stepKeys(op); !slices.Equal(keys, []string{"hold", "wipe", "format", "place", "fix", "verify", "sync", "release"}) || op.Steps[5].State != "skipped" {
		t.Fatalf("steps %v (verify %s)", keys, op.Steps[5].State)
	}

	// Files where nothing is mounted: something's wrong, so it stops.
	if _, err := a.stepPlaceParity(ctx, op, func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Fatalf("non-empty mount point: %v", err)
	}
	os.RemoveAll(r.path("mnt/parity1"))

	// Before the new disk takes over, Cancel changes nothing.
	if msg, err := a.undoAssign(ctx, op); err != nil || !strings.Contains(msg, "still down") {
		t.Fatalf("undo before place: %q %v", msg, err)
	}

	// The new disk won't mount: /etc/fstab goes back as it was.
	r.failMount["/mnt/parity1:new-uuid"] = true
	if _, err := a.stepPlaceParity(ctx, op, func(string) {}); err == nil || !strings.Contains(err.Error(), "back as it was") || readText(r.path("etc/fstab")) != testFstab {
		t.Fatalf("failed place: %v\n%s", err, readText(r.path("etc/fstab")))
	}
	r.failMount["/mnt/parity1:new-uuid"] = false
	if _, err := a.stepPlaceParity(ctx, op, func(string) {}); err != nil || r.mounted["/mnt/parity1"] != "new-uuid" {
		t.Fatalf("place: %v %v", err, r.mounted)
	}
	if !strings.Contains(readText(r.path("etc/fstab")), "UUID=new-uuid  /mnt/parity1  ext4  defaults,nofail  0  2") {
		t.Fatalf("fstab:\n%s", readText(r.path("etc/fstab")))
	}
	if d, _ := a.stepPlaceParity(ctx, op, func(string) {}); d != "Already in place." {
		t.Fatalf("place again: %q", d)
	}

	// Now only finishing gets parity back, so Cancel is refused, even if
	// the mount went away.
	if _, err := a.undoAssign(ctx, op); err == nil || !strings.Contains(err.Error(), "Continue instead") {
		t.Fatalf("undo after place: %v", err)
	}
	delete(r.mounted, "/mnt/parity1")
	if _, err := a.undoAssign(ctx, op); err == nil {
		t.Fatal("undo allowed with the new disk in /etc/fstab")
	}
}

func TestAssignReasons(t *testing.T) {
	const tb = uint64(1e12)
	data := []ArraySlot{{Pool: "disk1", Size: 16 * tb}, {Pool: "disk2", Size: 12 * tb}}
	par := []ParitySlot{{Name: "parity", Label: "Parity 1", Mounted: true, Size: 16 * tb, Used: 11 * tb}}
	check := func(name string, c Candidate, data_, parity_, swap string) {
		t.Helper()
		got := assignReasons(c, data, par)
		for _, x := range [][2]string{{got.NotData, data_}, {got.NotParity, parity_}, {got.NotSwap["parity"], swap}} {
			if (x[1] == "") != (x[0] == "") || !strings.Contains(x[0], x[1]) {
				t.Errorf("%s: data %q, parity %q, swap %q", name, got.NotData, got.NotParity, got.NotSwap["parity"])
				return
			}
		}
	}
	check("18 TB: parity yes, data no", Candidate{Size: 18 * tb}, "Bigger than Parity 1", "", "")
	check("16 TB: anything", Candidate{Size: 16 * tb}, "", "", "")
	check("14 TB: data only", Candidate{Size: 14 * tb}, "", "Smaller than disk1", "Smaller than Parity 1's disk")
	check("in use", Candidate{Size: 16 * tb, Blocked: "In use: mounted at /boot."}, "In use", "In use", "In use")
	check("USB", Candidate{Size: 16 * tb, Transport: "usb"}, "USB disks", "USB disks", "USB disks")
	// Room for the parity file matters when the disks are the same size.
	par[0].Used = 16 * tb
	check("no room for the parity file", Candidate{Size: 16 * tb}, "", "", "Too small for Parity 1's parity file")

	// A parity disk that's down is rebuilt from the data disks, so only
	// their size counts (not the old disk's, nor its parity file's), and it
	// doesn't cap the size of data disks.
	par[0] = ParitySlot{Name: "parity", Label: "Parity 1", Size: 18 * tb, Used: 16 * tb}
	check("down: 16 TB rebuilds it", Candidate{Size: 16 * tb}, "", "", "")
	check("down: 14 TB is too small for the data", Candidate{Size: 14 * tb}, "", "Smaller than disk1", "Smaller than disk1")
}

func TestParityNames(t *testing.T) {
	for name, lvl := range map[string]int{"parity": 1, "2-parity": 2, "q-parity": 2, "z-parity": 3, "6-parity": 6, "7-parity": 0, "data": 0} {
		if got := parityLevel(name); got != lvl {
			t.Errorf("parityLevel(%q) = %d, want %d", name, got, lvl)
		}
	}
	if parityName(1) != "parity" || parityName(3) != "3-parity" || parityFileName(2) != "snapraid.2-parity" {
		t.Error("parityName / parityFileName")
	}
	if !covers("/mnt/cache:/mnt/disk*", "/mnt/disk7") || covers("/mnt/disk1:/mnt/disk2=RW", "/mnt/disk3") || !covers("/mnt/disk3=NC", "/mnt/disk3") {
		t.Error("covers")
	}
	if got := replaceField("/mnt/a:/mnt/b  /mnt/user\tfuse.mergerfs  opts 0 0", 0, "/mnt/a:/mnt/b:/mnt/c"); got != "/mnt/a:/mnt/b:/mnt/c  /mnt/user\tfuse.mergerfs  opts 0 0" {
		t.Errorf("replaceField kept the spacing badly: %q", got)
	}
}

func TestAddDataSteps(t *testing.T) {
	a, r, branches := assignHost(t)
	ctx := context.Background()
	op := &ReplaceOp{ID: "job1", Method: "add-data", Snap: "d3", Pool: "disk3", Path: "/mnt/disk3", Folders: []string{"Backups", "Media", "Music"},
		NewDevice: "/dev/disk/by-id/ata-NEW_DISK_NEW1", NewSerial: "NEW1"}
	op.Steps = assignSteps(op)
	if keys := stepKeys(op); !slices.Equal(keys, []string{"hold", "wipe", "create", "folders", "config", "mergerfs", "sync", "release"}) {
		t.Fatalf("steps %v", keys)
	}

	// Where it would go, and that each view can keep it after a restart.
	changes, err := a.mergerPlan(ctx, []string{"/mnt/disk1", "/mnt/disk2"}, "/mnt/disk3")
	if err != nil || len(changes) != 2 || changes[0].Target != "/mnt/user" || !changes[0].Live ||
		changes[0].NewSrc != "/mnt/cache:/mnt/disk1:/mnt/disk2:/mnt/disk3" || changes[1].NewSrc != "/mnt/disk1:/mnt/disk2:/mnt/disk3" {
		t.Fatalf("plan: %+v %v", changes, err)
	}

	// The share folders, owned like on the other disks.
	os.MkdirAll(r.path("mnt/disk3"), 0o700)
	r.mounted["/mnt/disk3"] = ""
	if detail, err := a.stepFolders(ctx, op, func(string) {}); err != nil || !strings.Contains(detail, "3 folders") {
		t.Fatalf("folders: %q %v", detail, err)
	}
	for _, f := range op.Folders {
		if fi, err := os.Stat(r.path("mnt/disk3/" + f)); err != nil || !fi.IsDir() {
			t.Errorf("%s not created", f)
		}
	}
	if fi, _ := os.Stat(r.path("mnt/disk3")); fi.Mode().Perm() != 0o755 {
		t.Errorf("the disk's own folder is %v, not like disk1's", fi.Mode().Perm())
	}

	// snapraid.conf: after d2, written the same way, backed up, idempotent.
	if _, err := a.stepConfig(ctx, op, func(string) {}); err != nil {
		t.Fatal(err)
	}
	conf := readText(r.path("etc/snapraid.conf"))
	if !strings.Contains(conf, "data d2 /mnt/disk2/\ndata d3 /mnt/disk3/\n") {
		t.Fatalf("snapraid.conf:\n%s", conf)
	}
	if readText(r.path("etc/snapraid.conf.homestack-job1")) != testSnapConf {
		t.Fatal("no backup of snapraid.conf as it was")
	}
	if d, err := a.stepConfig(ctx, op, func(string) {}); err != nil || d != "Already in snapraid.conf." {
		t.Fatalf("again: %q %v", d, err)
	}

	// mergerfs: in /etc/fstab, then live; the unrelated view is left alone.
	if _, err := a.stepMergerfs(ctx, op, func(string) {}); err != nil {
		t.Fatal(err)
	}
	fstab := readText(r.path("etc/fstab"))
	if !strings.Contains(fstab, "/mnt/cache:/mnt/disk1:/mnt/disk2:/mnt/disk3  /mnt/user  fuse.mergerfs") ||
		!strings.Contains(fstab, "/mnt/disk1:/mnt/disk2:/mnt/disk3  /mnt/user-array") || readText(r.path("etc/fstab.homestack-job1")) != testFstab {
		t.Fatalf("fstab:\n%s", fstab)
	}
	if !strings.HasSuffix(branches[r.path("mnt/user/.mergerfs")], ":/mnt/disk3=RW") || strings.Contains(branches[r.path("mnt/backups/.mergerfs")], "disk3") {
		t.Fatalf("branches: %v", branches)
	}
	if _, err := a.stepMergerfs(ctx, op, func(string) {}); err != nil || strings.Count(readText(r.path("etc/fstab")), "/mnt/disk3") != 2 {
		t.Fatalf("again: %v\n%s", err, readText(r.path("etc/fstab")))
	}

	// Cancel: refused once a file is on it; otherwise everything comes out.
	r.out["zpool list -Hp"] = "disk3\t1000\t1\t999\t0%\t0%\tONLINE\n"
	r.out["zfs get -H -o value mountpoint,mounted disk3"] = "/mnt/disk3\nyes\n"
	r.out["zpool list -Ho name"] = "disk3\n"
	r.out["zpool list -vHP disk3"] = "disk3\t1000\t1\t999\n\t/dev/disk/by-id/ata-NEW_DISK_NEW1-part1\t1000\n"
	r.out["lsblk -no PKNAME /dev/disk/by-id/ata-NEW_DISK_NEW1-part1"] = "sdf\n"
	r.out["zpool destroy disk3"] = ""
	os.WriteFile(r.path("mnt/disk3/Media/film.mkv"), []byte("x"), 0o644)
	if _, err := a.undoAssign(ctx, op); err == nil || !strings.Contains(err.Error(), "already holds files (Media/film.mkv)") {
		t.Fatalf("undo with a file on it: %v", err)
	}
	if slices.Contains(r.ran, "zpool destroy disk3") {
		t.Fatal("destroyed a pool with a file on it")
	}
	os.Remove(r.path("mnt/disk3/Media/film.mkv"))
	if msg, err := a.undoAssign(ctx, op); err != nil || !strings.Contains(msg, "Undone") {
		t.Fatalf("undo: %q %v", msg, err)
	}
	if readText(r.path("etc/fstab")) != testFstab || readText(r.path("etc/snapraid.conf")) != testSnapConf ||
		strings.Contains(branches[r.path("mnt/user/.mergerfs")], "disk3") || !slices.Contains(r.ran, "zpool destroy disk3") {
		t.Fatalf("not undone:\n%s\n%s\n%v", readText(r.path("etc/fstab")), readText(r.path("etc/snapraid.conf")), branches)
	}
}

func TestMergerfsNeedsFstab(t *testing.T) {
	a, r, _ := assignHost(t)
	os.WriteFile(r.path("etc/fstab"), []byte("UUID=x / ext4 defaults 0 1\n/mnt/disk* /mnt/user-array fuse.mergerfs defaults 0 0\n"), 0o644)
	_, err := a.mergerPlan(context.Background(), []string{"/mnt/disk1", "/mnt/disk2"}, "/mnt/disk3")
	if err == nil || !strings.Contains(err.Error(), "/mnt/user isn't set up in /etc/fstab") {
		t.Fatalf("a view outside fstab must stop it: %v", err)
	}
	// A glob in fstab already covers it: nothing to write there.
	os.WriteFile(r.path("etc/fstab"), []byte("/mnt/cache:/mnt/disk* /mnt/user fuse.mergerfs defaults 0 0\n/mnt/disk* /mnt/user-array fuse.mergerfs defaults 0 0\n"), 0o644)
	changes, err := a.mergerPlan(context.Background(), []string{"/mnt/disk1", "/mnt/disk2"}, "/mnt/disk3")
	if err != nil || len(changes) != 2 || changes[0].NewSrc != "" || !changes[0].Live {
		t.Fatalf("glob: %+v %v", changes, err)
	}
}

func TestEditFstabChecks(t *testing.T) {
	a, r, _ := assignHost(t)
	op := &ReplaceOp{ID: "job2"}
	add := func(lines []string) ([]string, error) {
		return append(lines, "UUID=nope /mnt/parity2 ext4 defaults,nofail 0 2"), nil
	}

	// An error the old file didn't have: refused, nothing changed.
	r.out["findmnt --verify --tab-file "+r.path("etc/fstab.homestack-tmp")] = "!/mnt/parity2\n   [E] unreachable source: UUID=nope\n\n0 parse errors, 1 error, 0 warnings\n"
	err := a.editFstab(context.Background(), op, add)
	if err == nil || !strings.Contains(err.Error(), "didn't pass findmnt --verify (/mnt/parity2: unreachable source: UUID=nope)") {
		t.Fatalf("got %v", err)
	}
	if readText(r.path("etc/fstab")) != testFstab {
		t.Fatal("fstab changed anyway")
	}
	if _, err := os.Stat(r.path("etc/fstab.homestack-tmp")); err == nil {
		t.Fatal("the temporary file was left behind")
	}
	// The same error in the old file isn't held against the new one.
	r.out["findmnt --verify --tab-file "+r.path("etc/fstab")] = "!/mnt/parity2\n   [E] unreachable source: UUID=nope\n\n0 parse errors, 1 error, 0 warnings\n"
	if err := a.editFstab(context.Background(), op, add); err != nil {
		t.Fatalf("pre-existing error: %v", err)
	}
	// A check that can't run counts as failing.
	os.WriteFile(r.path("etc/fstab"), []byte(testFstab), 0o644)
	delete(r.out, "findmnt --verify --tab-file")
	delete(r.out, "findmnt --verify --tab-file "+r.path("etc/fstab"))
	if err := a.editFstab(context.Background(), op, add); err == nil || !strings.Contains(err.Error(), "couldn't check /etc/fstab") {
		t.Fatalf("unchecked: %v", err)
	}
}

func TestAddParitySteps(t *testing.T) {
	a, r, _ := assignHost(t)
	ctx := context.Background()
	dev := "/dev/disk/by-id/ata-NEW_DISK_NEW1"
	r.out["parted -s -a optimal "+dev+" mklabel gpt mkpart parity2 ext4 0% 100%"] = ""
	r.out["partprobe"] = ""
	r.out["udevadm settle"] = ""
	r.out["mkfs.ext4 -F -q -m 0 -T largefile4 -L parity2 "+dev+"-part1"] = ""
	r.out["blkid -s UUID -o value "+dev+"-part1"] = "new-uuid\n"
	op := &ReplaceOp{ID: "job3", Method: "add-parity", Snap: "2-parity", Pool: "parity 2", Path: "/mnt/parity2", Level: 2,
		ParityFile: "/mnt/parity2/snapraid.2-parity", FSType: "ext4", MountOpts: "defaults,nofail",
		NewDevice: dev, NewModel: "NEW DISK", NewSerial: "NEW1"}
	op.Steps = assignSteps(op)
	if keys := stepKeys(op); !slices.Equal(keys, []string{"hold", "wipe", "format", "mount", "config", "sync", "release"}) {
		t.Fatalf("steps %v", keys)
	}

	if _, err := a.stepFormat(ctx, op, func(string) {}); err != nil || op.NewUUID != "new-uuid" {
		t.Fatalf("format: %v %q", err, op.NewUUID)
	}
	if d, _ := a.stepFormat(ctx, op, func(string) {}); d != "Already formatted." {
		t.Fatalf("format again: %q", d)
	}
	if _, err := a.stepMountParity(ctx, op, func(string) {}); err != nil || r.mounted["/mnt/parity2"] != "new-uuid" {
		t.Fatalf("mount: %v %v", err, r.mounted)
	}
	fstab := readText(r.path("etc/fstab"))
	if !strings.Contains(fstab, "# parity 2: NEW DISK NEW1, added by Homestack") || !strings.Contains(fstab, "UUID=new-uuid  /mnt/parity2  ext4  defaults,nofail  0  2\n") {
		t.Fatalf("fstab:\n%s", fstab)
	}
	if d, _ := a.stepMountParity(ctx, op, func(string) {}); d != "Already mounted." {
		t.Fatalf("mount again: %q", d)
	}
	if _, err := a.stepConfig(ctx, op, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if conf := readText(r.path("etc/snapraid.conf")); !strings.Contains(conf, "parity /mnt/parity1/snapraid.parity\n2-parity /mnt/parity2/snapraid.2-parity\n") {
		t.Fatalf("snapraid.conf:\n%s", conf)
	}

	// Cancel takes it all back out, leaving the files as they were.
	if msg, err := a.undoAssign(ctx, op); err != nil || !strings.Contains(msg, "Undone") {
		t.Fatalf("undo: %q %v", msg, err)
	}
	if readText(r.path("etc/fstab")) != testFstab || readText(r.path("etc/snapraid.conf")) != testSnapConf || r.mounted["/mnt/parity2"] != "" {
		t.Fatalf("not undone:\n%s\n%s", readText(r.path("etc/fstab")), readText(r.path("etc/snapraid.conf")))
	}
}

func TestReplaceParitySteps(t *testing.T) {
	a, r, _ := assignHost(t)
	ctx := context.Background()
	op := &ReplaceOp{ID: "job4", Method: "replace-parity", Snap: "parity", Pool: "parity 1", Path: "/mnt/parity1", TempMount: "/mnt/parity1-new",
		ParityFile: "/mnt/parity1/snapraid.parity", FSType: "ext4", OldUUID: "old-uuid", NewUUID: "new-uuid", OldSerial: "OLD1"}
	op.Steps = assignSteps(op)
	if keys := stepKeys(op); !slices.Equal(keys, []string{"hold", "wipe", "format", "copy", "swap", "sync", "release"}) {
		t.Fatalf("steps %v", keys)
	}
	if _, err := a.stepCopyParity(ctx, op, func(string) {}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snapraid.parity", "snapraid.content"} {
		x, _ := os.ReadFile(r.path("mnt/parity1/" + f))
		y, _ := os.ReadFile(r.path("mnt/parity1-new/" + f))
		if len(x) == 0 || !bytes.Equal(x, y) {
			t.Fatalf("%s wasn't copied exactly", f)
		}
	}
	// Before the swap, Cancel leaves parity on the old disk.
	if msg, err := a.undoAssign(ctx, op); err != nil || !strings.Contains(msg, "stays on its old disk") || r.mounted["/mnt/parity1-new"] != "" {
		t.Fatalf("undo before the swap: %q %v", msg, err)
	}

	// The new disk won't mount: the old one goes back.
	r.failMount["/mnt/parity1:new-uuid"] = true
	_, err := a.stepSwapParity(ctx, op, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "so /etc/fstab is back as it was") || r.mounted["/mnt/parity1"] != "old-uuid" || readText(r.path("etc/fstab")) != testFstab {
		t.Fatalf("failed swap: %v, mounted %v\n%s", err, r.mounted, readText(r.path("etc/fstab")))
	}
	r.failMount["/mnt/parity1:new-uuid"] = false
	if _, err := a.stepSwapParity(ctx, op, func(string) {}); err != nil || r.mounted["/mnt/parity1"] != "new-uuid" {
		t.Fatalf("swap: %v %v", err, r.mounted)
	}
	if !strings.Contains(readText(r.path("etc/fstab")), "UUID=new-uuid  /mnt/parity1  ext4  defaults,nofail  0  2") {
		t.Fatalf("fstab:\n%s", readText(r.path("etc/fstab")))
	}
	if d, err := a.stepSwapParity(ctx, op, func(string) {}); err != nil || !strings.Contains(d, "mounted from the new disk") {
		t.Fatalf("swap again: %q %v", d, err)
	}

	// Halfway through a swap (parity unmounted), Cancel is refused.
	delete(r.mounted, "/mnt/parity1")
	if _, err := a.undoAssign(ctx, op); err == nil || !strings.Contains(err.Error(), "Continue to finish it") {
		t.Fatalf("undo mid-swap: %v", err)
	}
}

func TestCopyResumable(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	data := make([]byte, 5<<20+123)
	rand.Read(data)
	os.WriteFile(src, data, 0o640)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(src, old, old)

	// Stopped before it starts: nothing lost, and it says where it got to.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyResumable(ctx, src, dst, func(int64) {}); !errors.Is(err, errStopped) {
		t.Fatalf("cancelled: %v", err)
	}
	// Resumed from a saved checkpoint: only what's after it is read again.
	os.WriteFile(dst+partSuffix, append(slices.Clone(data[:2<<20]), []byte("torn tail, never synced")...), 0o600)
	os.WriteFile(dst+partSuffix+".ok", []byte("2097152"), 0o600)
	var first int64 = -1
	if err := copyResumable(context.Background(), src, dst, func(n int64) {
		if first < 0 {
			first = n
		}
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	fi, _ := os.Stat(dst)
	if !bytes.Equal(got, data) || first != 2<<20 || !fi.ModTime().Equal(old) || fi.Mode().Perm() != 0o640 {
		t.Fatalf("resumed copy: equal %v, started at %d, mtime %v, mode %v", bytes.Equal(got, data), first, fi.ModTime(), fi.Mode())
	}
	if _, err := os.Stat(dst + partSuffix + ".ok"); err == nil {
		t.Fatal("checkpoint left behind")
	}
	// Done already: not copied again.
	if err := copyResumable(context.Background(), src, dst, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	// A checkpoint past the end of the part file starts over.
	os.Remove(dst)
	os.WriteFile(dst+partSuffix, data[:100], 0o600)
	os.WriteFile(dst+partSuffix+".ok", []byte("999999999"), 0o600)
	if err := copyResumable(context.Background(), src, dst, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("bad checkpoint: copy differs")
	}
}

func TestFilesOn(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Media"), 0o755)
	os.MkdirAll(filepath.Join(root, ".zfs/snapshot"), 0o755)
	if f := filesOn(root, []string{"Media"}); f != "" {
		t.Fatalf("empty folders and .zfs count as empty, got %q", f)
	}
	os.WriteFile(filepath.Join(root, "notes.txt"), nil, 0o644)
	if f := filesOn(root, []string{"Media"}); f != "notes.txt" {
		t.Fatalf("got %q", f)
	}
}

func stepKeys(op *ReplaceOp) []string {
	var k []string
	for _, s := range op.Steps {
		k = append(k, s.Key)
	}
	return k
}
