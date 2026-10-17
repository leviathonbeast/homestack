package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"homestack/internal/notify"
)

// Replacing a data disk: Unraid's "stop array, unassign, assign the new disk,
// start" for ZFS single-disk pools plus SnapRAID, as a guided job.
//
// Two methods, picked from the state of the old disk:
//
//   - rebuild: the old disk is gone or failed. A new pool with the same name
//     and mount point goes on the new disk, `snapraid fix -d dN` restores its
//     files from parity, `snapraid check -a` verifies them, permissions are
//     reset (SnapRAID doesn't keep owners), then parity is synced.
//   - copy: the old disk still works (a bigger disk, or one failing SMART).
//     `zpool replace` copies it to the new disk while it stays in use. Parity
//     isn't needed and stays valid, since the files don't change.
//
// The job's progress lives in replace.json so it carries on after a restart,
// and a hold file makes the mover and the parity schedules skip until it's
// done: a sync halfway through a rebuild would tell parity that the files
// not yet restored were deleted.

const (
	holdName    = "storage-hold"
	replaceName = "replace.json"
	knownName   = "array-disks.json"
)

// ---------- The hold ----------

func (a *Agent) holdPath() string { return filepath.Join(a.opt.StateDir, holdName) }

// hold says why storage jobs are paused ("" when they aren't).
func (a *Agent) hold() string {
	b, err := os.ReadFile(a.holdPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (a *Agent) setHold(reason string) error {
	return os.WriteFile(a.holdPath(), []byte(reason+"\n"), 0o644)
}

func (a *Agent) clearHold() { _ = os.Remove(a.holdPath()) }

// ---------- What each data disk looked like when it last worked ----------

// KnownDisk is remembered so a disk that has vanished can still be described
// ("was a WD 12TB, serial D7JVXP9N, 6.7 TB used").
type KnownDisk struct {
	Device   string    `json:"device"`
	Model    string    `json:"model"`
	Serial   string    `json:"serial"`
	Size     uint64    `json:"size"`
	PoolSize uint64    `json:"poolSize"`
	Used     uint64    `json:"used"`
	Seen     time.Time `json:"seen"`
}

var knownMu sync.Mutex

func (a *Agent) loadKnown() map[string]KnownDisk {
	knownMu.Lock()
	defer knownMu.Unlock()
	m := map[string]KnownDisk{}
	if b, err := os.ReadFile(filepath.Join(a.opt.StateDir, knownName)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// rememberDisks records the disk behind every healthy array pool, and every
// mounted data disk that isn't ZFS. Written only when something changed, or
// hourly for the usage figures.
func (a *Agent) rememberDisks(pools []Pool, disks []Disk, slots []ArraySlot) {
	type held struct {
		name       string
		size, used uint64
	}
	var hs []held
	for _, p := range pools {
		if p.Health == "ONLINE" {
			hs = append(hs, held{p.Name, p.Size, p.Alloc})
		}
	}
	for _, s := range slots {
		if s.plain() && s.Mounted {
			hs = append(hs, held{s.Pool, s.PoolSize, s.Used})
		}
	}
	known := a.loadKnown()
	changed := false
	for _, h := range hs {
		var d *Disk
		for i := range disks {
			if disks[i].Role == h.name {
				d = &disks[i]
			}
		}
		if d == nil || d.Serial == "" {
			continue
		}
		k := KnownDisk{Device: a.byID(d.Name), Model: d.Model, Serial: d.Serial, Size: d.Size, PoolSize: h.size, Used: h.used, Seen: time.Now()}
		old, ok := known[h.name]
		if !ok || old.Serial != k.Serial || old.Device != k.Device || old.Size != k.Size || time.Since(old.Seen) > time.Hour {
			known[h.name] = k
			changed = true
		}
	}
	if !changed {
		return
	}
	knownMu.Lock()
	defer knownMu.Unlock()
	b, _ := json.MarshalIndent(known, "", "  ")
	tmp := filepath.Join(a.opt.StateDir, knownName+".tmp")
	if os.WriteFile(tmp, b, 0o640) == nil {
		_ = os.Rename(tmp, filepath.Join(a.opt.StateDir, knownName))
	}
}

// ---------- Device names ----------

// byID finds the stable /dev/disk/by-id name of a disk (sdc -> ata-...).
func (a *Agent) byID(dev string) string {
	dir := a.sys("/dev/disk/by-id")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	rank := func(n string) int {
		switch {
		case strings.HasPrefix(n, "ata-"):
			return 0
		case strings.HasPrefix(n, "nvme-") && !strings.HasPrefix(n, "nvme-eui.") && !strings.HasPrefix(n, "nvme-nvme."):
			return 1
		case strings.HasPrefix(n, "scsi-SATA_"), strings.HasPrefix(n, "scsi-3"):
			return 2
		case strings.HasPrefix(n, "scsi-"):
			return 3
		case strings.HasPrefix(n, "wwn-"):
			return 4
		}
		return 5
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if rePart.MatchString(n) {
			continue
		}
		if t, err := filepath.EvalSymlinks(filepath.Join(dir, n)); err == nil && filepath.Base(t) == dev {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Slice(names, func(i, j int) bool {
		if rank(names[i]) != rank(names[j]) {
			return rank(names[i]) < rank(names[j])
		}
		return names[i] < names[j]
	})
	return "/dev/disk/by-id/" + names[0]
}

// devName resolves a /dev/disk/by-id path to the kernel name (sdc), or "".
func (a *Agent) devName(byid string) string {
	if byid == "" {
		return ""
	}
	t, err := filepath.EvalSymlinks(a.sys(byid))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

var rePart = regexp.MustCompile(`-part\d+$`)

// ---------- The data disks ----------

// ArraySlot is one data disk in snapraid.conf and the pool behind it.
type ArraySlot struct {
	Snap         string `json:"snap"`  // d3
	Path         string `json:"path"`  // /mnt/disk3
	Pool         string `json:"pool"`  // disk3
	State        string `json:"state"` // ONLINE, FAULTED, UNAVAIL, SUSPENDED, …, MISSING, IMPORTABLE
	Mounted      bool   `json:"mounted"`
	Size         uint64 `json:"size"` // the disk (current or last known)
	PoolSize     uint64 `json:"poolSize"`
	Used         uint64 `json:"used"`
	Device       string `json:"device,omitempty"`
	DevName      string `json:"devName,omitempty"`
	Model        string `json:"model,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Vdev         string `json:"vdev,omitempty"` // the device path ZFS uses (for zpool replace)
	Remembered   bool   `json:"remembered,omitempty"`
	SmartFailing bool   `json:"smartFailing,omitempty"`
	FSType       string `json:"fsType,omitempty"` // zfs, or ext4, xfs… (plaindisks.go); "" unknown
	Method       string `json:"method,omitempty"` // rebuild | copy | "" (can't be replaced now)
	Why          string `json:"why"`
}

func (s ArraySlot) plain() bool { return s.FSType != "" && s.FSType != "zfs" }

func (s ArraySlot) healthy() bool { return s.State == "ONLINE" && s.Mounted }

// slots lists the data disks. withImport also looks for pools that are
// connected but not imported (slow: it scans every disk).
func (a *Agent) slots(ctx context.Context, pools []Pool, disks []Disk, ps ParityStatus, withImport bool) []ArraySlot {
	conf, err := readSnapConf(a.snapraidConf())
	if err != nil {
		return []ArraySlot{}
	}
	known := a.loadKnown()
	fstab := a.readFstab()
	var importable map[string]bool
	out := []ArraySlot{}
	for _, d := range conf.data {
		path := strings.TrimRight(d.Path, "/")
		s := ArraySlot{Snap: d.Name, Path: path, Pool: filepath.Base(path), State: "MISSING"}
		var pool *Pool
		for i := range pools {
			if pools[i].Mountpoint == path {
				pool = &pools[i]
			}
		}
		if pool == nil {
			for i := range pools {
				if pools[i].Name == s.Pool {
					pool = &pools[i]
				}
			}
		}
		if pool != nil {
			s.Pool, s.State, s.Mounted, s.PoolSize, s.Used, s.FSType = pool.Name, pool.Health, pool.Mounted, pool.Size, pool.Alloc, "zfs"
			if len(pool.Devices) == 1 {
				s.Vdev = pool.Devices[0]
			}
		} else if fs, f := a.dataFS(ctx, path, fstab); fs != "" && fs != "zfs" {
			// Not a ZFS pool (ext4, XFS…): working while it's mounted, and
			// still there (not mounted) while the array is stopped.
			s.FSType = fs
			if f != nil {
				s.State, s.Mounted, s.PoolSize, s.Used = "ONLINE", true, f.Size, f.Used
			} else if a.fstabDevicePresent(ctx, path, fstab) {
				s.State = "ONLINE"
			}
		}
		for _, dk := range disks {
			if dk.Role == s.Pool {
				s.DevName, s.Model, s.Serial, s.Size = dk.Name, dk.Model, dk.Serial, dk.Size
				s.Device = a.byID(dk.Name)
				s.SmartFailing = dk.Health == "failed"
			}
		}
		if k, ok := known[s.Pool]; ok {
			if s.Serial == "" {
				s.Device, s.Model, s.Serial, s.Size, s.Remembered = k.Device, k.Model, k.Serial, k.Size, true
			}
			if s.Used == 0 {
				s.Used, s.PoolSize = k.Used, k.PoolSize
			}
		}
		if s.State == "MISSING" && withImport {
			if importable == nil {
				importable = a.importablePools(ctx)
			}
			if importable[s.Pool] {
				s.State = "IMPORTABLE"
			}
		}
		out = append(out, s)
	}
	for i := range out {
		out[i].Method, out[i].Why = methodFor(out[i], out, ps)
	}
	return out
}

// methodFor decides how a disk can be replaced, or why it can't be yet.
func methodFor(s ArraySlot, all []ArraySlot, ps ParityStatus) (string, string) {
	if s.plain() {
		return "", fmt.Sprintf("%s is %s, not a ZFS pool, and Homestack only replaces ZFS data disks. By hand: shut down, put the new disk in, format it %s and mount it at %s (fstab), then rebuild its files from parity: sudo snapraid -d %s -l fix.log fix", s.Pool, s.FSType, s.FSType, s.Path, s.Snap)
	}
	switch s.State {
	case "IMPORTABLE":
		return "", fmt.Sprintf("The disk is connected, but its pool isn't imported, so nothing needs replacing yet. Import it with `sudo zpool import %s`, then check it on this page.", s.Pool)
	case "ONLINE":
		return "copy", "It still works, so ZFS copies it straight to the new disk while it stays in use. Parity isn't needed and stays valid. The new disk goes in alongside it, so it needs a free SATA port. No free port? Shut down and swap the disks, and it's rebuilt from parity instead."
	}
	// Rebuilding needs parity and every other disk.
	if !ps.Configured || !ps.Synced {
		return "", "It can't be rebuilt: there's no parity to rebuild it from."
	}
	for _, p := range ps.Parity {
		if !p.Mounted {
			return "", "The parity disk isn't mounted, so there's nothing to rebuild from. Get it back first."
		}
	}
	for _, o := range all {
		if o.Snap != s.Snap && !o.healthy() {
			return "", fmt.Sprintf("%s isn't working either (%s). Single parity can rebuild one missing disk, not two. Get %s back first (check its cable and power), then rebuild this one.", o.Pool, strings.ToLower(o.State), o.Pool)
		}
	}
	return "rebuild", "Its files are rebuilt from parity onto the new disk, as of the last parity sync. Anything written to it since then can't be brought back."
}

var reImportPool = regexp.MustCompile(`(?m)^\s*pool:\s*(\S+)`)

func (a *Agent) importablePools(ctx context.Context) map[string]bool {
	out, _ := a.cmd(ctx, 20*time.Second, "zpool", "import")
	m := map[string]bool{}
	for _, mm := range reImportPool.FindAllStringSubmatch(out, -1) {
		m[mm[1]] = true
	}
	return m
}

// ---------- Disks that could be the new one ----------

// Candidate is a disk that isn't part of the array, cache, parity or system.
type Candidate struct {
	Name       string `json:"name"`
	Device     string `json:"device"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	Size       uint64 `json:"size"`
	Rotational bool   `json:"rotational"`
	Transport  string `json:"transport"`
	Contents   string `json:"contents"`          // "Empty", or what's on it now
	Blocked    string `json:"blocked,omitempty"` // why it can't be used
	Warn       string `json:"warn,omitempty"`
}

type lsblkNode struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	FSType      any         `json:"fstype"`
	Label       any         `json:"label"`
	Mountpoints []any       `json:"mountpoints"`
	Children    []lsblkNode `json:"children"`
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// diskUsage looks at what's on a disk. blocked is set when it's in use.
func (a *Agent) diskUsage(ctx context.Context, name string, arrayPools map[string]bool) (contents, blocked, warn string, parts []string) {
	o, err := a.cmd(ctx, 10*time.Second, "lsblk", "-J", "-b", "-o", "NAME,TYPE,FSTYPE,LABEL,MOUNTPOINTS", "/dev/"+name)
	if err != nil {
		return "", "Couldn't read the disk (lsblk failed).", "", nil
	}
	var ls struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	if json.Unmarshal([]byte(o), &ls) != nil || len(ls.Blockdevices) == 0 {
		return "", "Couldn't read the disk.", "", nil
	}
	var found []string
	var walk func(n lsblkNode, top bool)
	walk = func(n lsblkNode, top bool) {
		if !top && n.Type == "part" {
			parts = append(parts, n.Name)
		}
		for _, m := range n.Mountpoints {
			if mp := str(m); mp != "" && blocked == "" {
				blocked = "In use: mounted at " + mp + "."
			}
		}
		fs, label := str(n.FSType), str(n.Label)
		switch fs {
		case "":
		case "linux_raid_member", "LVM2_member", "crypto_LUKS":
			if blocked == "" {
				blocked = "In use: part of a RAID, LVM or encrypted volume."
			}
		case "zfs_member":
			if arrayPools[label] && blocked == "" {
				blocked = fmt.Sprintf("This has the ZFS pool %q on it, so it looks like the old %s. If it's the disk that dropped out, import it instead of replacing it: sudo zpool import %s", label, label, label)
			}
			found = append(found, fmt.Sprintf("ZFS pool %q", label))
		default:
			d := fs
			if label != "" {
				d += fmt.Sprintf(" %q", label)
			}
			found = append(found, d)
		}
		for _, c := range n.Children {
			walk(c, false)
		}
	}
	walk(ls.Blockdevices[0], true)
	switch {
	case len(found) > 0:
		contents = "Has " + strings.Join(found, ", ")
		warn = "Everything on it is erased."
	case len(parts) > 0:
		contents = fmt.Sprintf("%d empty partition%s", len(parts), map[bool]string{true: "", false: "s"}[len(parts) == 1])
		warn = "Its partitions are erased."
	default:
		contents = "Empty"
	}
	return contents, blocked, warn, parts
}

func (a *Agent) candidates(ctx context.Context, disks []Disk, slots []ArraySlot) []Candidate {
	arrayPools := map[string]bool{}
	for _, s := range slots {
		arrayPools[s.Pool] = true
	}
	reserved := "" // the disk a disk job is setting up
	if op := a.opNow(); op.active() {
		reserved = op.NewDevice
	}
	out := []Candidate{}
	for _, d := range disks {
		if d.Role != "" {
			continue
		}
		c := Candidate{Name: d.Name, Model: d.Model, Serial: d.Serial, Size: d.Size, Rotational: d.Rotational, Transport: d.Transport}
		if reserved != "" && a.byID(d.Name) == reserved {
			c.Device, c.Blocked = reserved, "The disk job in progress is setting it up."
			out = append(out, c)
			continue
		}
		c.Device = a.byID(d.Name)
		c.Contents, c.Blocked, c.Warn, _ = a.diskUsage(ctx, d.Name, arrayPools)
		if c.Device == "" && c.Blocked == "" {
			c.Blocked = "It has no stable name in /dev/disk/by-id."
		}
		if c.Serial == "" && c.Blocked == "" {
			c.Blocked = "It doesn't report a serial number."
		}
		if d.Health == "failed" && c.Blocked == "" {
			c.Blocked = "It's failing SMART itself."
		}
		out = append(out, c)
	}
	return out
}

// ---------- The replacement job ----------

// ReplaceStep is one step of the job.
type ReplaceStep struct {
	Key      string     `json:"key"`
	Title    string     `json:"title"`
	State    string     `json:"state"` // pending, running, done, skipped, attention, failed
	Detail   string     `json:"detail,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
}

// ReplaceOp is a replacement from start to finish (replace.json).
type ReplaceOp struct {
	ID         string   `json:"id"`
	Method     string   `json:"method"`
	Snap       string   `json:"snap"`
	Pool       string   `json:"pool"`
	Path       string   `json:"path"`
	OldDevice  string   `json:"oldDevice,omitempty"`
	OldVdev    string   `json:"oldVdev,omitempty"`
	OldModel   string   `json:"oldModel,omitempty"`
	OldSerial  string   `json:"oldSerial,omitempty"`
	OldSize    uint64   `json:"oldSize,omitempty"`
	Used       uint64   `json:"used,omitempty"`
	NewDevice  string   `json:"newDevice"`
	NewModel   string   `json:"newModel"`
	NewSerial  string   `json:"newSerial"`
	NewSize    uint64   `json:"newSize"`
	CreateArgs []string `json:"createArgs,omitempty"` // rebuild: zpool create options from a healthy pool
	Template   string   `json:"template,omitempty"`
	FixPerms   bool     `json:"fixPerms"`
	Accepted   bool     `json:"accepted,omitempty"` // the user accepted unrecoverable files
	// Assigning a disk (assign.go): add-data, add-parity, replace-parity.
	Level         int           `json:"level,omitempty"`      // parity level
	ParityFile    string        `json:"parityFile,omitempty"` // /mnt/parity2/snapraid.2-parity
	FSType        string        `json:"fsType,omitempty"`     // a parity disk's filesystem
	MountOpts     string        `json:"mountOpts,omitempty"`
	OldUUID       string        `json:"oldUUID,omitempty"`   // the parity disk being replaced
	NewUUID       string        `json:"newUUID,omitempty"`   // the new parity disk's filesystem
	TempMount     string        `json:"tempMount,omitempty"` // the new parity disk, mounted for the copy
	Folders       []string      `json:"folders,omitempty"`   // share folders for a new data disk
	Verify        bool          `json:"verify,omitempty"`    // rebuild-parity: check it afterwards
	Backups       []string      `json:"backups,omitempty"`   // files as they were before the job changed them
	Steps         []ReplaceStep `json:"steps"`
	State         string        `json:"state"` // running, paused, attention, failed, done, cancelled
	Message       string        `json:"message,omitempty"`
	Unrecoverable []string      `json:"unrecoverable,omitempty"`
	Started       time.Time     `json:"started"`
	Finished      *time.Time    `json:"finished,omitempty"`
}

func (op *ReplaceOp) active() bool {
	return op != nil && op.State != "done" && op.State != "cancelled"
}

// current is the first step that isn't finished.
func (op *ReplaceOp) current() int {
	for i, s := range op.Steps {
		if s.State != "done" && s.State != "skipped" {
			return i
		}
	}
	return len(op.Steps)
}

func (op *ReplaceOp) stepDone(key string) bool {
	for _, s := range op.Steps {
		if s.Key == key {
			return s.State == "done" || s.State == "skipped"
		}
	}
	return false
}

func stepsFor(method, pool string, fixPerms bool) []ReplaceStep {
	mk := func(k, t string) ReplaceStep { return ReplaceStep{Key: k, Title: t, State: "pending"} }
	if method == "copy" {
		return []ReplaceStep{
			mk("hold", "Pause the mover and parity jobs"),
			mk("wipe", "Prepare the new disk"),
			mk("replace", "Start copying "+pool+" to the new disk"),
			mk("resilver", "Copy everything (resilver)"),
			mk("expand", "Use the new disk's full size"),
			mk("release", "Resume the mover and parity jobs"),
		}
	}
	s := []ReplaceStep{
		mk("hold", "Pause the mover and parity jobs"),
		mk("remove-old", "Remove the old "+pool+" pool"),
		mk("wipe", "Prepare the new disk"),
		mk("create", "Create "+pool+" on the new disk"),
		mk("fix", "Rebuild the files from parity"),
		mk("audit", "Check the rebuilt files"),
		mk("perms", "Reset owners and permissions"),
		mk("sync", "Update parity"),
		mk("release", "Resume the mover and parity jobs"),
	}
	if !fixPerms {
		s[6].State, s[6].Detail = "skipped", "Not chosen."
	}
	return s
}

var replaceMu sync.Mutex

func (a *Agent) loadOp() *ReplaceOp {
	replaceMu.Lock()
	defer replaceMu.Unlock()
	b, err := os.ReadFile(filepath.Join(a.opt.StateDir, replaceName))
	if err != nil {
		return nil
	}
	var op ReplaceOp
	if json.Unmarshal(b, &op) != nil {
		return nil
	}
	return &op
}

func (a *Agent) saveOp(op *ReplaceOp) {
	replaceMu.Lock()
	defer replaceMu.Unlock()
	b, _ := json.MarshalIndent(op, "", "  ")
	p := filepath.Join(a.opt.StateDir, replaceName)
	if os.WriteFile(p+".tmp", b, 0o640) == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

// opNow is the saved job, with "running" corrected when no job is running
// (the agent or the server restarted halfway).
func (a *Agent) opNow() *ReplaceOp {
	op := a.loadOp()
	if op != nil && op.State == "running" && a.jobs.runningAction() != "disk-replace" {
		op.State = "paused"
		op.Message = "Interrupted: the server or the agent restarted. Press Continue to carry on from here."
		if time.Since(a.started) < 3*time.Minute {
			op.Message = "The server or the agent just restarted; this carries on by itself in a minute or two, once the disks are mounted."
		}
		for i := range op.Steps {
			if op.Steps[i].State == "running" {
				op.Steps[i].State = "pending"
			}
		}
	}
	return op
}

// ReplaceSummary is the short version for the Storage page.
type ReplaceSummary struct {
	Pool    string `json:"pool"`
	Method  string `json:"method"`
	State   string `json:"state"`
	Step    string `json:"step"`
	Message string `json:"message,omitempty"`
}

func (a *Agent) replaceSummary() *ReplaceSummary {
	op := a.opNow()
	if !op.active() {
		return nil
	}
	s := &ReplaceSummary{Pool: op.Pool, Method: op.Method, State: op.State, Message: op.Message}
	if i := op.current(); i < len(op.Steps) {
		s.Step = op.Steps[i].Title
	}
	return s
}

// ---------- API ----------

// ReplaceView is everything the Replace disk page shows.
type ReplaceView struct {
	Slots        []ArraySlot `json:"slots"`
	Candidates   []Candidate `json:"candidates"`
	Op           *ReplaceOp  `json:"op"`
	ParitySize   uint64      `json:"paritySize"`
	ParitySynced bool        `json:"paritySynced"`
	LastSync     *time.Time  `json:"lastSync,omitempty"`
	Busy         string      `json:"busy,omitempty"`
	Hold         string      `json:"hold,omitempty"`
	Running      *Job        `json:"running,omitempty"`
	LastJob      *Job        `json:"lastJob,omitempty"` // the latest replacement job (for its log)
	Owner        string      `json:"owner"`             // who owns the shares' files, for people
}

func (a *Agent) replaceView(ctx context.Context) ReplaceView {
	pools := a.pools(ctx)
	disks := a.disks.snapshot()
	ps := a.parityStatus(ctx)
	v := ReplaceView{Op: a.opNow(), ParitySynced: ps.Synced, LastSync: ps.LastSync, Busy: a.jobs.lockHolder(), Hold: a.hold(), Owner: a.layout().ownerName()}
	v.Slots = a.slots(ctx, pools, disks, ps, true)
	v.Candidates = a.candidates(ctx, disks, v.Slots)
	for _, d := range disks {
		if d.Role == "parity" && d.Size > v.ParitySize {
			v.ParitySize = d.Size
		}
	}
	hist, running := a.jobs.snapshot()
	v.Running = running
	for i := range hist {
		if hist[i].Action == "disk-replace" {
			v.LastJob = &hist[i]
			break
		}
	}
	if running != nil && running.Action == "disk-replace" {
		v.LastJob = running
	}
	return v
}

type replaceReq struct {
	Action   string `json:"action"` // start, continue, accept, cancel, dismiss
	Method   string `json:"method"`
	Snap     string `json:"snap"`
	Device   string `json:"device"`
	Confirm  string `json:"confirm"`
	FixPerms bool   `json:"fixPerms"`
}

func (a *Agent) getReplace(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.replaceView(r.Context()))
}

func (a *Agent) postReplace(w http.ResponseWriter, r *http.Request) {
	var req replaceReq
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	ctx := r.Context()
	var err error
	switch req.Action {
	case "start":
		err = a.replaceStart(ctx, req)
	case "continue", "accept":
		err = a.replaceContinue(ctx, req.Action == "accept", req.Confirm)
	case "cancel":
		err = a.replaceCancel(ctx)
	case "dismiss":
		err = a.replaceDismiss()
	default:
		err = refuse("Unknown action.")
	}
	if err != nil {
		if rf, ok := errors.AsType[refusal](err); ok {
			writeErr(w, 409, rf.msg)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	a.refresh(context.Background())
	writeJSON(w, 200, a.replaceView(ctx))
}

func (a *Agent) notBusy(ctx context.Context) error {
	if h := a.jobs.lockHolder(); h != "" {
		return refuse("Busy: %s is running. Wait for it to finish, or stop it first.", h)
	}
	if a.snapraidRunning(ctx) {
		return refuse("Busy: SnapRAID is running in a terminal (outside Homestack). Wait for it to finish.")
	}
	return nil
}

func (a *Agent) replaceStart(ctx context.Context, req replaceReq) error {
	if op := a.opNow(); op.active() {
		return refuse("%s is in progress. One disk job at a time: finish or cancel that first.", opTitle(op))
	}
	if err := a.notBusy(ctx); err != nil {
		return err
	}
	v := a.replaceView(ctx)
	var slot *ArraySlot
	for i := range v.Slots {
		if v.Slots[i].Snap == req.Snap {
			slot = &v.Slots[i]
		}
	}
	if slot == nil {
		return refuse("There's no data disk %q in snapraid.conf.", req.Snap)
	}
	if slot.Method == "" {
		return refuse("%s", slot.Why)
	}
	if req.Method != slot.Method {
		return refuse("%s can't be replaced that way right now (the page was out of date). Reload and try again.", slot.Pool)
	}
	var c *Candidate
	for i := range v.Candidates {
		if v.Candidates[i].Device == req.Device && req.Device != "" {
			c = &v.Candidates[i]
		}
	}
	if c == nil {
		return refuse("That disk isn't available any more. Reload the page.")
	}
	if c.Blocked != "" {
		return refuse("%s", c.Blocked)
	}
	if strings.TrimSpace(req.Confirm) != c.Serial {
		return refuse("Type the new disk's serial number (%s) to confirm.", c.Serial)
	}
	switch slot.Method {
	case "copy":
		if c.Size < slot.Size {
			return refuse("The new disk (%s) is smaller than %s (%s). ZFS can only copy onto a disk at least as big.", human(c.Size), slot.Pool, human(slot.Size))
		}
		if slot.Vdev == "" {
			return refuse("Couldn't work out which device %s uses. Check `zpool status -P %s`.", slot.Pool, slot.Pool)
		}
	case "rebuild":
		if slot.Used > 0 && float64(c.Size)*0.95 < float64(slot.Used) {
			return refuse("The new disk (%s) is too small for the %s that was on %s.", human(c.Size), human(slot.Used), slot.Pool)
		}
	}
	op := &ReplaceOp{
		ID: newID(), Method: slot.Method, Snap: slot.Snap, Pool: slot.Pool, Path: slot.Path,
		OldDevice: slot.Device, OldVdev: slot.Vdev, OldModel: slot.Model, OldSerial: slot.Serial, OldSize: slot.Size, Used: slot.Used,
		NewDevice: c.Device, NewModel: c.Model, NewSerial: c.Serial, NewSize: c.Size,
		FixPerms: req.FixPerms, State: "running", Started: time.Now(),
	}
	if op.Method == "rebuild" {
		tmpl := ""
		for _, s := range v.Slots {
			if s.healthy() && s.Pool != op.Pool {
				tmpl = s.Pool
				break
			}
		}
		if tmpl == "" {
			return refuse("There's no healthy pool to copy the settings from.")
		}
		args, err := a.templateArgs(ctx, tmpl)
		if err != nil {
			return err
		}
		op.Template, op.CreateArgs = tmpl, args
	}
	op.Steps = stepsFor(op.Method, op.Pool, op.FixPerms)
	a.saveOp(op)
	a.notifyHost(notify.Warning, opTitle(op), startMessage(op))
	return a.runReplaceJob()
}

// templateArgs copies a healthy pool's settings for zpool create.
func (a *Agent) templateArgs(ctx context.Context, pool string) ([]string, error) {
	var args []string
	ashift := "12"
	if o, err := a.cmd(ctx, 10*time.Second, "zpool", "get", "-H", "-o", "value", "ashift", pool); err == nil {
		if v := strings.TrimSpace(o); v != "" && v != "0" && v != "-" {
			ashift = v
		}
	}
	args = append(args, "-o", "ashift="+ashift)
	if o, err := a.cmd(ctx, 10*time.Second, "zpool", "get", "-H", "-o", "value", "compatibility", pool); err == nil {
		if v := strings.TrimSpace(o); v != "" && v != "off" && v != "-" {
			args = append(args, "-o", "compatibility="+v)
		}
	}
	o, err := a.cmd(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "property,value", "-s", "local", "all", pool)
	if err != nil {
		return nil, refuse("Couldn't read %s's settings: %s", pool, strings.TrimSpace(o))
	}
	for line := range strings.SplitSeq(strings.TrimSpace(o), "\n") {
		f := strings.SplitN(line, "\t", 2)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "mountpoint", "readonly":
			continue
		case "encryption", "keylocation", "keyformat":
			return nil, refuse("%s is encrypted; set up the new pool by hand.", pool)
		}
		args = append(args, "-O", f[0]+"="+f[1])
	}
	// Creation-only settings don't show as "local".
	if o, err := a.cmd(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "property,value", "casesensitivity,normalization,utf8only", pool); err == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(o), "\n") {
			f := strings.SplitN(line, "\t", 2)
			if len(f) != 2 {
				continue
			}
			def := map[string]string{"casesensitivity": "sensitive", "normalization": "none", "utf8only": "off"}[f[0]]
			if f[1] != def && f[1] != "-" && f[1] != "" {
				args = append(args, "-O", f[0]+"="+f[1])
			}
		}
	}
	return args, nil
}

func (a *Agent) replaceContinue(ctx context.Context, accept bool, confirm string) error {
	op := a.opNow()
	if !op.active() {
		return refuse("There's no replacement to continue.")
	}
	if a.jobs.runningAction() == "disk-replace" {
		return refuse("It's already running.")
	}
	if err := a.notBusy(ctx); err != nil {
		return err
	}
	i := op.current()
	if accept {
		if op.State != "attention" || i >= len(op.Steps) {
			return refuse("There's nothing waiting for a decision.")
		}
		if confirm != "ACCEPT" {
			return refuse("Type ACCEPT to confirm.")
		}
		op.Accepted = true
		op.Steps[i].State = "done"
		first := op.Steps[i].Detail
		if j := strings.Index(first, ": "); j > 0 {
			first = first[:j]
		}
		op.Steps[i].Detail = "Accepted by you: " + strings.TrimSuffix(first, ".") + "."
		if op.Steps[i].Key == "fix" && op.Method == "rebuild" {
			// The check would only list the same files again.
			for j := range op.Steps {
				if op.Steps[j].Key == "audit" && op.Steps[j].State == "pending" {
					op.Steps[j].State, op.Steps[j].Detail = "skipped", "Skipped: the unrecoverable files were accepted."
				}
			}
		}
	}
	if i < len(op.Steps) && (op.Steps[i].State == "failed" || op.Steps[i].State == "attention") {
		op.Steps[i].State = "pending"
	}
	op.State, op.Message = "running", ""
	a.saveOp(op)
	return a.runReplaceJob()
}

func (a *Agent) replaceCancel(ctx context.Context) error {
	op := a.opNow()
	if !op.active() {
		return refuse("There's no replacement to cancel.")
	}
	if a.jobs.runningAction() == "disk-replace" {
		return refuse("Stop it first.")
	}
	if op.Method == "rebuild" && op.stepDone("create") && !op.stepDone("sync") {
		return refuse("A rebuild can't be cancelled once the new disk is in place: until it finishes, parity is the only complete copy of %s's files. Continue it instead (Stop pauses it at any time).", op.Pool)
	}
	if op.Method == "copy" && op.stepDone("replace") && !op.stepDone("resilver") {
		// Abort the copy: detach the new disk; the old one carries on as before.
		if out, err := a.cmd(ctx, 2*time.Minute, "zpool", "detach", op.Pool, op.NewDevice); err != nil {
			return refuse("Couldn't stop the copy: %s", strings.TrimSpace(out))
		}
	}
	// Adding or moving a disk is undone (or refused when that isn't safe).
	undone, err := a.undoAssign(ctx, op)
	if err != nil {
		return err
	}
	now := time.Now()
	op.State, op.Finished = "cancelled", &now
	op.Message = "Cancelled."
	if op.Method == "rebuild" && op.stepDone("remove-old") {
		op.Message = fmt.Sprintf("Cancelled. %s is still missing; parity still covers its files.", op.Pool)
	}
	if undone != "" {
		op.Message = "Cancelled. " + undone
	}
	a.saveOp(op)
	a.clearHold()
	return nil
}

func (a *Agent) replaceDismiss() error {
	op := a.loadOp()
	if op == nil {
		return nil
	}
	if op.active() {
		return refuse("It isn't finished.")
	}
	p := filepath.Join(a.opt.StateDir, replaceName)
	return os.Rename(p, filepath.Join(a.opt.StateDir, "replace-last.json"))
}

// resumeReplace carries on after a restart, when the job was running (not
// stopped by you or waiting for a decision). Waits for the array to mount.
func (a *Agent) resumeReplace(ctx context.Context, delay time.Duration) {
	op := a.loadOp()
	if op == nil || op.State != "running" {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	if op := a.loadOp(); op == nil || op.State != "running" || a.jobs.runningAction() != "" {
		return
	}
	if err := a.runReplaceJob(); err != nil {
		op.State, op.Message = "paused", "Couldn't carry on by itself after the restart: "+err.Error()
		a.saveOp(op)
	}
}

func (a *Agent) runReplaceJob() error {
	op := a.loadOp()
	_, err := a.jobs.runFunc("disk-replace", opTitle(op), func(ctx context.Context, log func(string)) error {
		return a.runReplace(ctx, log)
	})
	return err
}

// errAttention stops the job to wait for a decision.
type errAttention struct{ msg string }

func (e errAttention) Error() string { return e.msg }

var errStopped = errors.New("stopped")

func (a *Agent) runReplace(ctx context.Context, log func(string)) error {
	op := a.loadOp()
	if op == nil {
		return errors.New("the replacement's state file is missing")
	}
	op.State, op.Message = "running", ""
	a.saveOp(op)
	for {
		i := op.current()
		if i >= len(op.Steps) {
			break
		}
		st := &op.Steps[i]
		now := time.Now()
		st.State, st.Started, st.Finished, st.Detail = "running", &now, nil, ""
		a.saveOp(op)
		log(st.Title + "…")
		detail, err := a.replaceStep(ctx, op, st.Key, log)
		end := time.Now()
		st.Finished = &end
		var att errAttention
		switch {
		case err == nil:
			st.State, st.Detail = "done", detail
			if detail != "" {
				log("  " + detail)
			}
		case errors.Is(err, errStopped) || a.jobs.stopWanted():
			st.State, st.Finished, st.Detail = "pending", nil, ""
			op.State, op.Message = "paused", "Stopped. Press Continue to carry on from here."
			if op.Method == "copy" && st.Key == "resilver" {
				op.Message = "Stopped watching. ZFS keeps copying in the background; press Continue to follow it again, or Cancel to abort the copy."
			}
			if a.jobs.stopByAgent() {
				// The agent is stopping (restart, update, shutdown): stay
				// "running" so it carries on by itself when it's back.
				op.State, op.Message = "running", ""
			}
			a.saveOp(op)
			return errStopped
		case errors.As(err, &att):
			st.State, st.Detail = "attention", att.msg
			op.State, op.Message = "attention", att.msg
			a.saveOp(op)
			a.notifyHost(notify.Error, opTitle(op)+": needs you", att.msg)
			return errors.New("waiting for your decision on the Replace disk page")
		default:
			st.State, st.Detail = "failed", err.Error()
			op.State, op.Message = "failed", err.Error()
			a.saveOp(op)
			a.notifyHost(notify.Error, opTitle(op)+" stopped", st.Title+" failed: "+err.Error())
			return err
		}
		a.saveOp(op)
	}
	now := time.Now()
	op.State, op.Finished = "done", &now
	title, msg := doneMessage(op, now.Sub(op.Started))
	op.Message = msg
	a.saveOp(op)
	a.notifyHost(notify.Success, title, op.Message+" The mover and parity jobs are back on.")
	log(op.Message)
	return nil
}

// replaceStep runs one step. Every step is safe to run again, so Continue
// after a restart simply repeats the one that was interrupted.
func (a *Agent) replaceStep(ctx context.Context, op *ReplaceOp, key string, log func(string)) (string, error) {
	// Steps that change disks run to the end even if Stop is pressed: only
	// SnapRAID (which stops cleanly on SIGINT), the permissions walk and the
	// resilver watch can be interrupted.
	switch key {
	case "fix", "audit", "perms", "sync", "resilver", "copy", "verify":
	default:
		ctx = context.WithoutCancel(ctx)
	}
	switch key {
	case "hold":
		why := opTitle(op)
		if how := map[string]string{"rebuild": "rebuilt from parity", "copy": "copied to a new disk"}[op.Method]; how != "" {
			why = fmt.Sprintf("%s is being %s", op.Pool, how)
		}
		if err := a.setHold(fmt.Sprintf("%s (started %s).", why, op.Started.Format("2 Jan 15:04"))); err != nil {
			return "", err
		}
		return "The nightly mover and parity jobs skip until this is finished.", nil
	case "remove-old":
		return a.stepRemoveOld(ctx, op, log)
	case "wipe":
		return a.stepWipe(ctx, op, log)
	case "create":
		return a.stepCreate(ctx, op, log)
	case "fix":
		if op.Method == "rebuild-parity" {
			return a.stepFixParity(ctx, op, log)
		}
		return a.stepFix(ctx, op, log)
	case "place":
		return a.stepPlaceParity(ctx, op, log)
	case "verify":
		return a.stepVerifyParity(ctx, op, log)
	case "audit":
		return a.stepAudit(ctx, op, log)
	case "perms":
		var detail string
		err := a.fixPermissions(ctx, op.Path, func(s string) {
			detail = s
			if strings.HasSuffix(s, "…") { // progress; the summary is logged as the step's result
				log("  " + s)
			}
		})
		if ctx.Err() != nil {
			return "", errStopped
		}
		return detail, err
	case "sync":
		return a.stepSync(ctx, op, log)
	case "replace":
		return a.stepZpoolReplace(ctx, op, log)
	case "resilver":
		return a.stepResilver(ctx, op, log)
	case "expand":
		if out, err := a.cmd(ctx, time.Minute, "zpool", "online", "-e", op.Pool, op.NewDevice); err != nil {
			return "", fmt.Errorf("zpool online -e: %s", strings.TrimSpace(out))
		}
		p, err := a.poolByName(ctx, op.Pool)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s is now %s.", op.Pool, human(p.Size)), nil
	case "format":
		return a.stepFormat(ctx, op, log)
	case "mount":
		return a.stepMountParity(ctx, op, log)
	case "folders":
		return a.stepFolders(ctx, op, log)
	case "config":
		return a.stepConfig(ctx, op, log)
	case "mergerfs":
		return a.stepMergerfs(ctx, op, log)
	case "copy":
		return a.stepCopyParity(ctx, op, log)
	case "swap":
		return a.stepSwapParity(ctx, op, log)
	case "release":
		a.clearHold()
		return "The mover and parity jobs are back on their schedules.", nil
	}
	return "", fmt.Errorf("unknown step %q", key)
}

func (a *Agent) stepRemoveOld(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	p, err := a.poolByName(ctx, op.Pool)
	if err != nil {
		a.lockMountpoint(ctx, op.Path)
		return "It wasn't imported any more.", nil
	}
	if p.Health == "ONLINE" && p.Mounted {
		return "", fmt.Errorf("%s is working again (ONLINE), so it isn't rebuilt. Cancel, and replace it by copying instead", op.Pool)
	}
	for _, d := range p.Devices {
		if n := a.devName(rePart.ReplaceAllString(d, "")); n != "" && n == a.devName(op.NewDevice) {
			return "", fmt.Errorf("the old pool uses the disk picked as the new one")
		}
	}
	log("  zpool export -f " + op.Pool)
	if out, err := a.cmd(ctx, 2*time.Minute, "zpool", "export", "-f", op.Pool); err != nil {
		log("  export failed (" + strings.TrimSpace(out) + "), destroying the pool record instead")
		if out, err := a.cmd(ctx, 2*time.Minute, "zpool", "destroy", "-f", op.Pool); err != nil {
			return "", fmt.Errorf("couldn't remove the old pool (%s). Restart the server (the new disk isn't touched yet), then press Continue", strings.TrimSpace(out))
		}
	}
	if _, err := a.poolByName(ctx, op.Pool); err == nil {
		return "", fmt.Errorf("the old pool is still there. Restart the server, then press Continue")
	}
	a.lockMountpoint(ctx, op.Path)
	return "Removed. Its mount point is locked so nothing can write to the boot disk there meanwhile.", nil
}

// lockMountpoint makes an empty, unmounted mount point read-only.
func (a *Agent) lockMountpoint(ctx context.Context, path string) {
	if !a.isMounted(ctx, path) {
		_ = os.MkdirAll(a.sys(path), 0o755)
		_, _ = a.cmd(ctx, 10*time.Second, "chattr", "+i", path)
	}
}

// newDisk checks the new disk is still the one picked (by serial).
func (a *Agent) newDisk(op *ReplaceOp) (string, error) {
	name := a.devName(op.NewDevice)
	if name == "" {
		return "", fmt.Errorf("the new disk (%s, serial %s) isn't connected", op.NewModel, op.NewSerial)
	}
	for _, d := range a.disks.snapshot() {
		if d.Name == name {
			if d.Serial != op.NewSerial {
				return "", fmt.Errorf("%s is now a different disk (serial %s, expected %s)", op.NewDevice, d.Serial, op.NewSerial)
			}
			return name, nil
		}
	}
	// Not polled yet (just connected): trust by-id, which contains the serial.
	if !strings.Contains(op.NewDevice, op.NewSerial) {
		return "", fmt.Errorf("couldn't confirm the disk at %s is serial %s", op.NewDevice, op.NewSerial)
	}
	return name, nil
}

// inPool says whether the new disk already belongs to the pool.
func (a *Agent) inPool(ctx context.Context, pool, name string) bool {
	return a.roles(ctx)[name] == pool
}

func (a *Agent) stepWipe(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	name, err := a.newDisk(op)
	if err != nil {
		return "", err
	}
	if a.inPool(ctx, op.Pool, name) {
		return "Already part of " + op.Pool + ".", nil
	}
	if role := a.roles(ctx)[name]; role != "" {
		return "", fmt.Errorf("the new disk is in use as %s", role)
	}
	arrayPools := map[string]bool{}
	ps := a.parityStatus(ctx)
	for _, s := range a.slots(ctx, a.pools(ctx), a.disks.snapshot(), ps, false) {
		arrayPools[s.Pool] = true
	}
	_, blocked, _, parts := a.diskUsage(ctx, name, arrayPools)
	if blocked != "" {
		return "", errors.New(blocked)
	}
	for _, p := range parts {
		_, _ = a.cmd(ctx, 30*time.Second, "zpool", "labelclear", "-f", "/dev/"+p)
		if out, err := a.cmd(ctx, 30*time.Second, "wipefs", "-a", "/dev/"+p); err != nil {
			return "", fmt.Errorf("wipefs /dev/%s: %s", p, strings.TrimSpace(out))
		}
	}
	_, _ = a.cmd(ctx, 30*time.Second, "zpool", "labelclear", "-f", op.NewDevice)
	if out, err := a.cmd(ctx, 30*time.Second, "wipefs", "-a", op.NewDevice); err != nil {
		return "", fmt.Errorf("wipefs: %s", strings.TrimSpace(out))
	}
	_, _ = a.cmd(ctx, 30*time.Second, "udevadm", "settle")
	return fmt.Sprintf("Erased %s %s (/dev/%s).", op.NewModel, op.NewSerial, name), nil
}

func (a *Agent) stepCreate(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	name, err := a.newDisk(op)
	if err != nil {
		return "", err
	}
	if p, err := a.poolByName(ctx, op.Pool); err == nil {
		if !a.inPool(ctx, op.Pool, name) {
			return "", fmt.Errorf("a pool called %s exists already, on another disk", op.Pool)
		}
		if !p.Mounted {
			if out, err := a.cmd(ctx, time.Minute, "zfs", "mount", op.Pool); err != nil {
				return "", fmt.Errorf("zfs mount: %s", strings.TrimSpace(out))
			}
		}
		return "Already created.", nil
	}
	_, _ = a.cmd(ctx, 10*time.Second, "chattr", "-i", op.Path)
	args := append([]string{"create", "-f"}, op.CreateArgs...)
	args = append(args, "-m", op.Path, op.Pool, op.NewDevice)
	log("  zpool " + strings.Join(args, " "))
	if out, err := a.cmd(ctx, 2*time.Minute, "zpool", args...); err != nil {
		a.lockMountpoint(ctx, op.Path)
		return "", fmt.Errorf("zpool create: %s", strings.TrimSpace(out))
	}
	_, _ = a.cmd(ctx, 30*time.Second, "udevadm", "settle")
	p, err := a.poolByName(ctx, op.Pool)
	if err != nil || p.Health != "ONLINE" {
		return "", fmt.Errorf("the new pool isn't ONLINE")
	}
	if !a.isMounted(ctx, op.Path) {
		if out, err := a.cmd(ctx, time.Minute, "zfs", "mount", op.Pool); err != nil {
			return "", fmt.Errorf("zfs mount: %s", strings.TrimSpace(out))
		}
	}
	return fmt.Sprintf("%s created (settings copied from %s) and mounted at %s.", op.Pool, op.Template, op.Path), nil
}

var (
	reUnrecoverable = regexp.MustCompile(`(?i)(\d+) unrecoverable errors`)
	reErrorsTotal   = regexp.MustCompile(`(?m)^\s*(\d+) errors\s*$`)
	reDataErrors    = regexp.MustCompile(`(?m)^\s*(\d+) (data|io) errors`)
)

func (a *Agent) snapLog(name string) string {
	return filepath.Join(a.opt.LogDir, "replace-"+name+".log")
}

func (a *Agent) stepFix(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	if !a.isMounted(ctx, op.Path) {
		return "", fmt.Errorf("%s isn't mounted", op.Path)
	}
	out := a.snapLog("fix")
	rc, err := a.runSnapraid(ctx, out, "-d", op.Snap, "-l", a.snapLog("fix-detail"), "fix")
	if err != nil {
		return "", err
	}
	log(indent(tailSnapLog(out, 12)))
	if a.jobs.stopWanted() {
		return "", errStopped
	}
	text := readTail(out, 256<<10)
	recovered, unrec := countFixLines(out)
	op.Unrecoverable = unrec
	if m := reUnrecoverable.FindStringSubmatch(text); (m != nil && m[1] != "0") || len(unrec) > 0 {
		return "", errAttention{fmt.Sprintf("%d file%s couldn't be rebuilt: another disk had read errors there, or they changed after the last sync. SnapRAID saved the partial copies as *.unrecoverable. Reconnect anything that dropped out and press Try again, or accept the loss and finish.", len(unrec), plural(len(unrec)))}
	}
	if rc != 0 {
		return "", fmt.Errorf("snapraid fix exited with code %d: %s", rc, lastLines(out, 3))
	}
	return fmt.Sprintf("%d files and folders rebuilt.", recovered), nil
}

func (a *Agent) stepAudit(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	out := a.snapLog("check")
	rc, err := a.runSnapraid(ctx, out, "-d", op.Snap, "-a", "check")
	if err != nil {
		return "", err
	}
	log(indent(tailSnapLog(out, 8)))
	if a.jobs.stopWanted() {
		return "", errStopped
	}
	text := readTail(out, 64<<10)
	if rc == 0 && strings.Contains(text, "Everything OK") {
		return "Every rebuilt file matches its checksum.", nil
	}
	n := "Some"
	if m := reErrorsTotal.FindStringSubmatch(text); m != nil {
		n = m[1]
	}
	return "", errAttention{fmt.Sprintf("%s file(s) don't match the checksums saved at the last sync. The log lists them. Press Try again to rebuild them again, or accept and finish.", n)}
}

// diffCount is one of snapraid diff's totals ("removed", "updated"…).
func diffCount(text, kind string) int {
	m := regexp.MustCompile(`(?m)^\s*(\d+) ` + kind + `\s*$`).FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func (a *Agent) stepSync(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	ps := a.parityStatus(ctx)
	for _, s := range a.slots(ctx, a.pools(ctx), a.disks.snapshot(), ps, false) {
		if !s.healthy() {
			return "", fmt.Errorf("%s isn't healthy (%s), so parity is left alone", s.Pool, strings.ToLower(s.State))
		}
	}
	if !op.Accepted {
		out := a.snapLog("diff")
		if _, err := a.runSnapraid(ctx, out, "diff"); err != nil {
			return "", err
		}
		if a.jobs.stopWanted() {
			return "", errStopped
		}
		text := readTail(out, 64<<10)
		if !regexp.MustCompile(`(?m)^\s*\d+ equal`).MatchString(text) {
			return "", fmt.Errorf("snapraid diff failed: %s", lastLines(out, 3))
		}
		if removed := diffCount(text, "removed"); removed > a.deleteLimit() {
			return "", errAttention{fmt.Sprintf("%d files would be dropped from parity (more than the limit of %d). If files were deleted on purpose meanwhile, accept to sync anyway; otherwise find out what's missing first (`snapraid diff`).", removed, a.deleteLimit())}
		}
		if updated := diffCount(text, "updated"); updated > a.updateLimit() {
			return "", errAttention{fmt.Sprintf("%d files were modified since the last sync (more than the limit of %d). Parity still has the old versions: check they weren't encrypted or corrupted (`snapraid diff` lists them). If the changes were intended, accept to sync anyway.", updated, a.updateLimit())}
		}
	}
	out := a.snapLog("sync")
	started := time.Now()
	args := []string{"sync"}
	if op.Method == "add-parity" {
		// A new parity level is built from scratch; SnapRAID's manual names
		// --force-full for this. The parity already there keeps protecting.
		args = append(args, "-F")
	}
	rc, err := a.runSnapraid(ctx, out, args...)
	if err != nil {
		return "", err
	}
	log(indent(tailSnapLog(out, 6)))
	if a.jobs.stopWanted() {
		return "", errStopped
	}
	text := readTail(out, 64<<10)
	if rc != 0 || strings.Contains(text, "DANGER") || hasErrors(text) {
		return "", fmt.Errorf("snapraid sync failed (exit %d): %s", rc, lastLines(out, 3))
	}
	summary := fmt.Sprintf("Parity updated (%s). Took %s.", strings.ToLower(opTitle(op)[:1])+opTitle(op)[1:], humanDur(time.Since(started)))
	a.jobs.recordResult("parity-sync", JobResult{Finished: time.Now(), OK: true, Summary: summary})
	return "Parity is up to date again.", nil
}

func (a *Agent) stepZpoolReplace(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	name, err := a.newDisk(op)
	if err != nil {
		return "", err
	}
	p, err := a.poolByName(ctx, op.Pool)
	if err != nil {
		return "", fmt.Errorf("%s isn't imported", op.Pool)
	}
	if a.inPool(ctx, op.Pool, name) {
		return "Already started.", nil
	}
	if p.Health != "ONLINE" {
		return "", fmt.Errorf("%s is %s now. Cancel, and rebuild it from parity instead", op.Pool, p.Health)
	}
	ashift := "12"
	if o, err := a.cmd(ctx, 10*time.Second, "zpool", "get", "-H", "-o", "value", "ashift", op.Pool); err == nil {
		if v := strings.TrimSpace(o); v != "" && v != "0" && v != "-" {
			ashift = v
		}
	}
	_, _ = a.cmd(ctx, 10*time.Second, "zpool", "set", "autoexpand=on", op.Pool)
	args := []string{"replace", "-o", "ashift=" + ashift, op.Pool, op.OldVdev, op.NewDevice}
	log("  zpool " + strings.Join(args, " "))
	if out, err := a.cmd(ctx, 2*time.Minute, "zpool", args...); err != nil {
		return "", fmt.Errorf("zpool replace: %s", strings.TrimSpace(out))
	}
	return "ZFS is copying " + op.Pool + " to the new disk.", nil
}

var reHMS = regexp.MustCompile(`^(?:(\d+) days )?(\d+):(\d+):(\d+)$`)

func (a *Agent) stepResilver(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	oldName := a.devName(rePart.ReplaceAllString(op.OldVdev, ""))
	lastLog, started := time.Time{}, time.Now()
	for {
		p, err := a.poolByName(ctx, op.Pool)
		if err != nil {
			return "", fmt.Errorf("%s disappeared", op.Pool)
		}
		oldStill := false
		for _, d := range p.Devices {
			if d == op.OldVdev || (oldName != "" && a.devName(rePart.ReplaceAllString(d, "")) == oldName) {
				oldStill = true
			}
		}
		if p.ScanState == "resilvering" {
			a.jobs.setProgress(func(j *Job) {
				j.Progress = p.ScanPercent
				j.ETA, j.ETASecs = p.ScanETA, nil
				if m := reHMS.FindStringSubmatch(p.ScanETA); m != nil {
					d, _ := strconv.ParseInt(m[1], 10, 64)
					h, _ := strconv.ParseInt(m[2], 10, 64)
					mi, _ := strconv.ParseInt(m[3], 10, 64)
					se, _ := strconv.ParseInt(m[4], 10, 64)
					secs := d*86400 + h*3600 + mi*60 + se
					j.ETASecs = &secs
				}
				if p.ScanPercent != nil && op.Used > 0 {
					done := uint64(float64(op.Used) * *p.ScanPercent / 100)
					total := op.Used
					j.Done, j.Total = &done, &total
				}
			})
			if time.Since(lastLog) > 30*time.Minute {
				lastLog = time.Now()
				log("  " + p.Scan)
			}
		} else if !oldStill {
			a.jobs.setProgress(func(j *Job) { j.Progress, j.ETA, j.ETASecs, j.Done, j.Total = nil, "", nil, nil, nil })
			if p.Health != "ONLINE" || (p.Errors != "" && !strings.Contains(p.Errors, "No known data errors")) {
				return "", errAttention{fmt.Sprintf("The copy finished, but ZFS reports: %s %s. `sudo zpool status -v %s` lists any damaged files; restore each one from parity with `sudo snapraid fix -f <file>`, then accept.", p.Health, p.Errors, op.Pool)}
			}
			return "Copied. The old disk has been released and can be removed.", nil
		} else if low := strings.ToLower(p.Scan); strings.Contains(low, "resilvered") {
			// Finished, but the old disk is still attached: ZFS keeps it when
			// the copy found errors.
			return "", errAttention{fmt.Sprintf("The copy finished, but ZFS kept the old disk attached, which means it found errors (%s). Check `sudo zpool status -v %s`; once it's clean, `sudo zpool detach %s %s` releases the old disk. Then press Try again.", p.Errors, op.Pool, op.Pool, op.OldVdev)}
		} else if waited := time.Since(started); waited > 15*time.Minute {
			return "", fmt.Errorf("the copy never started (zpool status: %s)", p.Scan)
		}
		select {
		case <-ctx.Done():
			return "", errStopped
		case <-time.After(resilverPoll):
		}
	}
}

var resilverPoll = 10 * time.Second

// ---------- Running SnapRAID with live progress ----------

// runSnapraid runs snapraid with the output in logPath, feeding its progress
// to the running job. Stop and agent shutdown send SIGINT (jobManager), so it
// stops cleanly. rc is SnapRAID's exit code.
func (a *Agent) runSnapraid(ctx context.Context, logPath string, args ...string) (int, error) {
	a.releaseSnapshots(ctx)
	full := append([]string{"-c", a.snapraidConf()}, args...)
	return a.jobs.runLogged(ctx, logPath, "snapraid", full...)
}

// releaseSnapshots unmounts the ZFS snapshots SnapRAID 15 reads the data
// disks through (snapraid-scan, -pending, -stable), as snapraid-job does
// before each run: on the host, and copies in other mount namespaces. ZFS
// counts a snapshot as mounted if it's mounted anywhere, and SnapRAID leaves
// them mounted; a container started meanwhile keeps its own copy (Docker
// binds are rprivate by default). SnapRAID then can't open that disk's
// snapshots after renaming them ("Too many levels of symbolic links"), which
// stops a sync. Harmless: ZFS mounts a snapshot again when it's next read.
func (a *Agent) releaseSnapshots(ctx context.Context) {
	proc := a.sys("/proc")
	for _, mp := range snapshotMounts(filepath.Join(proc, "mounts")) {
		if out, err := a.cmd(ctx, 30*time.Second, "umount", mp); err != nil {
			log.Printf("couldn't unmount SnapRAID's snapshot %s: %s", mp, strings.TrimSpace(out))
		}
	}
	// systemd's sandboxed services follow the host's unmounts, so what's left
	// by now is held apart, usually by a container.
	self, _ := os.Readlink(filepath.Join(proc, "self/ns/mnt"))
	seen := map[string]bool{self: true}
	ents, _ := os.ReadDir(proc)
	for _, e := range ents {
		pid := e.Name()
		if strings.Trim(pid, "0123456789") != "" {
			continue
		}
		ns, err := os.Readlink(filepath.Join(proc, pid, "ns/mnt"))
		if err != nil || seen[ns] {
			continue
		}
		seen[ns] = true
		for _, mp := range snapshotMounts(filepath.Join(proc, pid, "mounts")) {
			comm := strings.TrimSpace(readText(filepath.Join(proc, pid, "comm")))
			if out, err := a.cmd(ctx, 30*time.Second, "umount", "-N", pid, mp); err != nil {
				log.Printf("couldn't unmount SnapRAID's snapshot %s held by %s (pid %s): %s", mp, comm, pid, strings.TrimSpace(out))
			} else {
				log.Printf("unmounted SnapRAID's snapshot %s held by %s (pid %s), which had kept its own copy", mp, comm, pid)
			}
		}
	}
}

// snapshotMounts lists the mount points of SnapRAID's snapshots in a mounts
// file (/proc/mounts, /proc/PID/mounts).
func snapshotMounts(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for l := range strings.Lines(string(b)) {
		if f := strings.Fields(l); len(f) > 1 && strings.Contains(f[1], "/.zfs/snapshot/snapraid-") {
			out = append(out, unescapeMounts(f[1]))
		}
	}
	return out
}

// unescapeMounts undoes /proc/mounts' octal escapes (a space is \040).
func unescapeMounts(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------- Small helpers ----------

func countFixLines(path string) (recovered int, unrec []string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 512<<20))
	for _, l := range strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' || r == '\r' }) {
		switch {
		case strings.HasPrefix(l, "recovered "):
			recovered++
		case strings.HasPrefix(l, "unrecoverable "):
			if len(unrec) < 500 {
				unrec = append(unrec, unescapeSnap(strings.TrimPrefix(l, "unrecoverable ")))
			}
		}
	}
	return recovered, unrec
}

func readTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(fi.Size()-n, 0)
	buf := make([]byte, fi.Size()-off)
	_, _ = f.ReadAt(buf, off)
	return strings.ReplaceAll(string(buf), "\r", "\n")
}

func hasErrors(text string) bool {
	for _, m := range reDataErrors.FindAllStringSubmatch(text, -1) {
		if m[1] != "0" {
			return true
		}
	}
	return false
}

// alertsInt is a number from the alerts settings (/etc/default/homestack-
// alerts), which snapraid-job reads too, so both use the same limits.
func (a *Agent) alertsInt(key string, def int) int {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `=["']?(\d+)`)
	if m := re.FindSubmatch([]byte(readText(a.alertsConf()))); m != nil {
		if v, err := strconv.Atoi(string(m[1])); err == nil {
			return v
		}
	}
	return def
}

// The sync safety limits: more files deleted or modified since the last
// sync than this, and it waits for you (an accident, or ransomware).
func (a *Agent) deleteLimit() int { return a.parityValue("SNAPRAID_DELETE_LIMIT") }
func (a *Agent) updateLimit() int { return a.parityValue("SNAPRAID_UPDATE_LIMIT") }

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// human is a short size for messages, in the 1024-based units the pages
// show (a 16 TB disk is 14.6 TB), so the two always agree.
func human(b uint64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.0f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%d MB", b>>20)
	}
	return fmt.Sprintf("%d KB", max(b>>10, 1)) // a backup: not "0 MB"
}

// humanDur is "3h 05m", "12 min" or "under a minute".
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// unescapeSnap undoes SnapRAID's backslash escaping of paths (`track\ 1.bin`).
func unescapeSnap(p string) string {
	var b strings.Builder
	esc := false
	for _, r := range p {
		if r == '\\' && !esc {
			esc = true
			continue
		}
		esc = false
		b.WriteRune(r)
	}
	return b.String()
}
