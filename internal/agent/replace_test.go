package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptRunner answers commands from a table (the first key that prefixes
// "name arg arg…" wins) and records what ran.
type scriptRunner struct {
	out map[string]string
	ran []string
}

func (r *scriptRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.ran = append(r.ran, line)
	best := ""
	for k := range r.out {
		if strings.HasPrefix(line, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return nil, errors.New("exit 1")
	}
	v := r.out[best]
	if strings.HasPrefix(v, "!") {
		return []byte(v[1:]), errors.New("exit 1")
	}
	return []byte(v), nil
}

func TestMethodFor(t *testing.T) {
	ps := ParityStatus{Configured: true, Synced: true, Parity: []ParityDisk{{Mounted: true}}}
	ok := ArraySlot{Snap: "d1", Pool: "disk1", State: "ONLINE", Mounted: true}
	missing := ArraySlot{Snap: "d3", Pool: "disk3", State: "MISSING"}
	all := []ArraySlot{ok, missing}

	if m, _ := methodFor(ok, all, ps); m != "copy" {
		t.Errorf("a working disk should be copied, got %q", m)
	}
	if m, _ := methodFor(missing, all, ps); m != "rebuild" {
		t.Errorf("a missing disk should be rebuilt, got %q", m)
	}
	// Two disks gone: single parity can't do it.
	two := []ArraySlot{ok, missing, {Snap: "d4", Pool: "disk4", State: "FAULTED"}}
	if m, why := methodFor(missing, two, ps); m != "" || !strings.Contains(why, "disk4") {
		t.Errorf("two missing disks must refuse, got %q %q", m, why)
	}
	// No parity yet.
	if m, _ := methodFor(missing, all, ParityStatus{Configured: true}); m != "" {
		t.Errorf("no parity must refuse, got %q", m)
	}
	// Parity disk not mounted.
	if m, _ := methodFor(missing, all, ParityStatus{Configured: true, Synced: true, Parity: []ParityDisk{{Mounted: false}}}); m != "" {
		t.Errorf("unmounted parity must refuse, got %q", m)
	}
	// Connected but not imported: import it, don't replace it.
	imp := ArraySlot{Snap: "d3", Pool: "disk3", State: "IMPORTABLE"}
	if m, why := methodFor(imp, []ArraySlot{ok, imp}, ps); m != "" || !strings.Contains(why, "zpool import disk3") {
		t.Errorf("importable pool must say import, got %q %q", m, why)
	}
}

func TestDiskUsage(t *testing.T) {
	r := &scriptRunner{out: map[string]string{
		"lsblk -J -b -o NAME,TYPE,FSTYPE,LABEL,MOUNTPOINTS /dev/sdx": `{"blockdevices":[{"name":"sdx","type":"disk","fstype":null,"label":null,"mountpoints":[null],"children":[{"name":"sdx1","type":"part","fstype":"zfs_member","label":"disk3","mountpoints":[null]},{"name":"sdx9","type":"part","fstype":null,"label":null,"mountpoints":[null]}]}]}`,
		"lsblk -J -b -o NAME,TYPE,FSTYPE,LABEL,MOUNTPOINTS /dev/sdy": `{"blockdevices":[{"name":"sdy","type":"disk","fstype":null,"label":null,"mountpoints":[null],"children":[{"name":"sdy1","type":"part","fstype":"ext4","label":"backup","mountpoints":["/mnt/disks/backup"]}]}]}`,
		"lsblk -J -b -o NAME,TYPE,FSTYPE,LABEL,MOUNTPOINTS /dev/sdz": `{"blockdevices":[{"name":"sdz","type":"disk","fstype":null,"label":null,"mountpoints":[null]}]}`,
		"lsblk -J -b -o NAME,TYPE,FSTYPE,LABEL,MOUNTPOINTS /dev/sdw": `{"blockdevices":[{"name":"sdw","type":"disk","fstype":null,"label":null,"mountpoints":[null],"children":[{"name":"sdw1","type":"part","fstype":"zfs_member","label":"oldpool","mountpoints":[null]}]}]}`,
	}}
	a := New(Options{Runner: r, StateDir: t.TempDir()})
	arr := map[string]bool{"disk1": true, "disk3": true}
	ctx := context.Background()

	_, blocked, _, parts := a.diskUsage(ctx, "sdx", arr)
	if !strings.Contains(blocked, "old disk3") || len(parts) != 2 {
		t.Errorf("the old disk3 must be refused: %q %v", blocked, parts)
	}
	if _, blocked, _, _ := a.diskUsage(ctx, "sdy", arr); !strings.Contains(blocked, "/mnt/disks/backup") {
		t.Errorf("a mounted disk must be refused: %q", blocked)
	}
	if c, blocked, warn, _ := a.diskUsage(ctx, "sdz", arr); c != "Empty" || blocked != "" || warn != "" {
		t.Errorf("empty disk: %q %q %q", c, blocked, warn)
	}
	if c, blocked, warn, _ := a.diskUsage(ctx, "sdw", arr); blocked != "" || !strings.Contains(c, "oldpool") || warn == "" {
		t.Errorf("a disk from another pool is usable, with a warning: %q %q %q", c, blocked, warn)
	}
}

func TestImportableAndTemplate(t *testing.T) {
	r := &scriptRunner{out: map[string]string{
		"zpool import":                                    "!   pool: disk3\n     id: 1234\n  state: ONLINE\n action: The pool can be imported.\n\n   pool: old\n     id: 99\n",
		"zpool get -H -o value ashift disk1":              "12\n",
		"zpool get -H -o value compatibility disk1":       "off\n",
		"zfs get -H -o property,value -s local all disk1": "mountpoint\t/mnt/disk1\ncompression\tlz4\natime\toff\nxattr\tsa\nacltype\tposixacl\n",
		"zfs get -H -o property,value casesensitivity":    "casesensitivity\tsensitive\nnormalization\tformD\nutf8only\ton\n",
	}}
	a := New(Options{Runner: r, StateDir: t.TempDir()})
	ctx := context.Background()
	m := a.importablePools(ctx)
	if !m["disk3"] || !m["old"] || len(m) != 2 {
		t.Errorf("importable: %v", m)
	}
	args, err := a.templateArgs(ctx, "disk1")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	want := "-o ashift=12 -O compression=lz4 -O atime=off -O xattr=sa -O acltype=posixacl -O normalization=formD -O utf8only=on"
	if got != want {
		t.Errorf("template args\n got %s\nwant %s", got, want)
	}
}

func TestByID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dev/disk/by-id")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(root, "dev/sdc"), nil, 0o644)
	os.WriteFile(filepath.Join(root, "dev/sdc1"), nil, 0o644)
	for _, n := range []string{"wwn-0x5000c500e6", "ata-ST16000NE000_ZL2PR26D"} {
		os.Symlink("../../sdc", filepath.Join(dir, n))
	}
	os.Symlink("../../sdc1", filepath.Join(dir, "ata-ST16000NE000_ZL2PR26D-part1"))
	a := New(Options{SysRoot: root, StateDir: t.TempDir()})
	if got := a.byID("sdc"); got != "/dev/disk/by-id/ata-ST16000NE000_ZL2PR26D" {
		t.Errorf("byID = %q", got)
	}
	if got := a.devName("/dev/disk/by-id/ata-ST16000NE000_ZL2PR26D"); got != "sdc" {
		t.Errorf("devName = %q", got)
	}
}

func TestStepsAndOpState(t *testing.T) {
	s := stepsFor("rebuild", "disk3", false)
	if len(s) != 9 || s[6].Key != "perms" || s[6].State != "skipped" {
		t.Fatalf("rebuild steps: %+v", s)
	}
	op := &ReplaceOp{Steps: s, State: "running"}
	op.Steps[0].State, op.Steps[1].State = "done", "done"
	if op.current() != 2 || !op.stepDone("remove-old") || op.stepDone("create") {
		t.Errorf("current/stepDone wrong")
	}
	if c := stepsFor("copy", "disk2", true); c[3].Key != "resilver" {
		t.Errorf("copy steps: %+v", c)
	}

	// A job left "running" by a restart reads as paused.
	a := New(Options{StateDir: t.TempDir()})
	a.saveOp(&ReplaceOp{Pool: "disk3", State: "running", Steps: []ReplaceStep{{Key: "fix", State: "running"}}})
	got := a.opNow()
	if got.State != "paused" || got.Steps[0].State != "pending" {
		t.Errorf("interrupted op: %+v", got)
	}
}

func TestCancelRules(t *testing.T) {
	a := New(Options{StateDir: t.TempDir(), Runner: &scriptRunner{out: map[string]string{}}})
	steps := stepsFor("rebuild", "disk3", true)
	for i := range 4 { // up to and including create
		steps[i].State = "done"
	}
	a.saveOp(&ReplaceOp{Method: "rebuild", Pool: "disk3", State: "paused", Steps: steps})
	if err := a.replaceCancel(context.Background()); err == nil || !strings.Contains(err.Error(), "can't be cancelled") {
		t.Errorf("cancelling a rebuild after create must be refused, got %v", err)
	}
	steps = stepsFor("rebuild", "disk3", true)
	steps[0].State, steps[1].State = "done", "done"
	a.setHold("test")
	a.saveOp(&ReplaceOp{Method: "rebuild", Pool: "disk3", State: "failed", Steps: steps})
	if err := a.replaceCancel(context.Background()); err != nil {
		t.Fatalf("cancel before create: %v", err)
	}
	if a.hold() != "" || a.loadOp().State != "cancelled" {
		t.Errorf("cancel should clear the hold and mark it cancelled")
	}
}

func TestFixOutputParsing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fix.log")
	os.WriteFile(p, []byte("Fixing...\n1%, 0 MB\rrecovered Media/a.mkv\nrecovered Media/b.mkv\nunrecoverable Media/c\\ d.mkv\n100% completed, 23 MB accessed in 0:00\nDANGER! Unrecoverable errors detected!\n\n      68 soft errors\n       0 io errors\n       0 data errors\n      54 recovered errors\n       7 UNRECOVERABLE errors\n"), 0o644)
	rec, unrec := countFixLines(p)
	if rec != 2 || len(unrec) != 1 || unrec[0] != "Media/c d.mkv" {
		t.Errorf("recovered %d, unrecoverable %v", rec, unrec)
	}
	if m := reUnrecoverable.FindStringSubmatch(readTail(p, 4096)); m == nil || m[1] != "7" {
		t.Errorf("unrecoverable count: %v", m)
	}
	if hasErrors(readTail(p, 4096)) {
		t.Errorf("0 io/data errors isn't an error")
	}
}

// The sync limits come from the alerts settings, like snapraid-job's.
func TestSyncLimits(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "homestack-alerts")
	t.Setenv("HOMESTACK_ALERTS_CONF", conf)
	a := New(Options{StateDir: dir, LogDir: dir})
	if a.deleteLimit() != 500 || a.updateLimit() != 1000 {
		t.Fatal("defaults")
	}
	os.WriteFile(conf, []byte("SNAPRAID_DELETE_LIMIT=50  # x\nSNAPRAID_UPDATE_LIMIT=\"2000\"\n"), 0o644)
	if a.deleteLimit() != 50 || a.updateLimit() != 2000 {
		t.Errorf("read %d %d", a.deleteLimit(), a.updateLimit())
	}
	diff := "  1000 equal\n     3 added\n   700 removed\n  1500 updated\n     0 moved\n     2 copied\n     9 relocated\n     0 restored\nThere are differences!\n"
	if diffCount(diff, "removed") != 700 || diffCount(diff, "updated") != 1500 || diffCount(diff, "relocated") != 9 || diffCount(diff, "moved") != 0 {
		t.Error("diffCount")
	}
}

// A skipped or failed sync doesn't hide when parity was last complete:
// Storage shows the problem next to the last good sync, not "no parity".
func TestLastOKSurvivesFailure(t *testing.T) {
	dir := t.TempDir()
	a := New(Options{StateDir: dir, LogDir: dir})
	good := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	a.jobs.recordResult("parity-sync", JobResult{Finished: good, OK: true, Summary: "Took 1h."})
	a.jobs.recordResult("parity-sync", JobResult{Finished: time.Now(), OK: false, Summary: "Skipped: 1500 files were modified (limit 1000)."})
	if r := a.jobs.lastOK("parity-sync"); r == nil || !r.Finished.Equal(good) {
		t.Errorf("last OK: %+v", r)
	}
	if r := a.jobs.lastResult("parity-sync"); r == nil || r.OK || !strings.HasPrefix(r.Summary, "Skipped:") {
		t.Errorf("last: %+v", r)
	}
	// Before last-ok files existed, an OK last-parity-sync.json still counts.
	os.Remove(filepath.Join(dir, "last-ok-parity-sync.json"))
	a.jobs.recordResult("parity-sync", JobResult{Finished: good, OK: true, Summary: "Took 1h."})
	os.Remove(filepath.Join(dir, "last-ok-parity-sync.json"))
	if r := a.jobs.lastOK("parity-sync"); r == nil {
		t.Error("an old OK result isn't read")
	}
}
