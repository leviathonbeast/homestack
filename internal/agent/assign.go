package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"homestack/internal/notify"
)

// Assigning disks, like Unraid's Main tab: the empty Disk N slot takes a
// spare disk into the array, the empty Parity N slot adds a parity level,
// and a parity disk can move to a new (bigger) disk. Each is a disk job on
// the replacement engine (replace.json, the hold, the Replace disk page
// for progress), so only one runs at a time, with the array up.
//
//   - add-data: a single-disk pool diskN at /mnt/diskN (Layout) with a working
//     pool's settings, the share folders (mergerfs's "existing path"
//     policies only put files where a folder exists), a "data dN" line in
//     snapraid.conf, a branch in each mergerfs view of the data disks (now,
//     and in /etc/fstab), then a parity sync.
//   - add-parity: partitioned and formatted like the first parity disk,
//     mounted at /mnt/parityN (Layout; in /etc/fstab, nofail), an "N-parity" line,
//     then sync -F builds the new level from every disk. The parity there
//     already keeps protecting meanwhile.
//   - replace-parity: formatted the same way, the files on the old parity
//     disk copied over (resumable), the new disk takes the old one's
//     /etc/fstab line, and a sync confirms parity. The old disk keeps its
//     copy until you reuse it.
//
// Foolproofing: planAssign checks everything it can before the first
// change, and the start checks it all again; the disk is confirmed by its
// serial; a file that's changed is backed up once per job and only replaced
// by a version that's been checked (findmnt --verify for /etc/fstab,
// reading snapraid.conf back); new mounts are nofail, so a missing disk
// can't hold up a boot; every step can run again; Cancel undoes the job,
// or refuses when that isn't safe.

const maxParity = 6 // SnapRAID's limit

// parityFS are the filesystems a parity disk is formatted with here.
var parityFS = map[string]bool{"ext4": true, "xfs": true}

// ---------- What's assigned ----------

// ParitySlot is one parity level in snapraid.conf and the disk behind it.
type ParitySlot struct {
	Name    string `json:"name"`  // parity, 2-parity
	Label   string `json:"label"` // Parity 2
	Level   int    `json:"level"`
	File    string `json:"file"`  // /mnt/parity1/snapraid.parity
	Mount   string `json:"mount"` // /mnt/parity1
	Mounted bool   `json:"mounted"`
	FSType  string `json:"fsType,omitempty"`
	UUID    string `json:"uuid,omitempty"`
	DevName string `json:"devName,omitempty"`
	Device  string `json:"device,omitempty"`
	Model   string `json:"model,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Size    uint64 `json:"size,omitempty"`
	Failing bool   `json:"failing,omitempty"` // SMART
	Used    uint64 `json:"used"`              // the parity file
	Source  string `json:"source,omitempty"`  // its /etc/fstab source, when it isn't mounted
}

// NextSlot is the empty slot at the end of the data disks or of parity.
type NextSlot struct {
	Name    string `json:"name"`  // d6, 2-parity
	Label   string `json:"label"` // Disk 6, Parity 2
	Pool    string `json:"pool,omitempty"`
	Path    string `json:"path"` // /mnt/disk6, /mnt/parity2
	Level   int    `json:"level,omitempty"`
	File    string `json:"file,omitempty"`
	Blocked string `json:"blocked,omitempty"` // why it can't take a disk now
}

// AssignCandidate is a spare disk, with why it can't fill each slot.
type AssignCandidate struct {
	Candidate
	NotData   string            `json:"notData,omitempty"`
	NotParity string            `json:"notParity,omitempty"`
	NotSwap   map[string]string `json:"notSwap"` // per parity level: why it can't replace that disk
}

// ArrayView is the Array devices card on the Storage page.
type ArrayView struct {
	Parity        []ParitySlot      `json:"parity"`
	Data          []ArraySlot       `json:"data"`
	Candidates    []AssignCandidate `json:"candidates"`
	NextData      *NextSlot         `json:"nextData,omitempty"`
	NextParity    *NextSlot         `json:"nextParity,omitempty"`
	Blocked       string            `json:"blocked,omitempty"`       // why nothing can change now
	ParityBlocked string            `json:"parityBlocked,omitempty"` // why parity can't change now
	Folders       []string          `json:"folders"`                 // share folders a new data disk gets
	Busy          string            `json:"busy,omitempty"`
	Op            *ReplaceSummary   `json:"op,omitempty"`
	Shares        string            `json:"shares"` // the shares' mount (Layout), for the wording
}

type assignPlan struct {
	view     ArrayView
	conf     *snapConf
	template string // a working pool to copy settings from
	fsType   string // for a new parity disk
	opts     string // its mount options
}

// planAssign works out the slots, the spare disks and what each can take.
func (a *Agent) planAssign(ctx context.Context) *assignPlan {
	p := &assignPlan{view: ArrayView{Parity: []ParitySlot{}, Data: []ArraySlot{}, Candidates: []AssignCandidate{}, Folders: []string{}, Shares: a.layout().Shares}}
	v := &p.view
	v.Op, v.Busy = a.replaceSummary(), a.jobs.lockHolder()
	conf, err := readSnapConf(a.snapraidConf())
	if err != nil {
		v.Blocked = fmt.Sprintf("SnapRAID isn't set up (%s is missing). The array's disks are listed there, so set it up first.", a.snapraidConf())
		return p
	}
	p.conf = conf
	pools := a.pools(ctx)
	disks := a.disks.snapshot()
	v.Data = a.slots(ctx, pools, disks, a.parityStatus(ctx), false)
	v.Parity = a.paritySlots(ctx, conf, disks)

	v.Blocked = assignBlocked(a.opNow(), v.Data, v.Parity, pools)
	for _, c := range a.candidates(ctx, disks, v.Data) {
		v.Candidates = append(v.Candidates, assignReasons(c, v.Data, v.Parity))
	}

	v.NextData = a.nextData(ctx, p, pools)
	v.NextParity, v.ParityBlocked = a.nextParity(ctx, p)
	for _, s := range v.Parity {
		if s.Mounted {
			continue
		}
		why := fmt.Sprintf("%s isn't mounted. Replace it (Replace on its row) or get it back first.", s.Label)
		for _, n := range []*NextSlot{v.NextData, v.NextParity} {
			if n != nil && n.Blocked == "" {
				n.Blocked = why
			}
		}
	}

	// The share folders: every top-level folder on the working data disks.
	seen := map[string]bool{}
	for _, s := range v.Data {
		if !s.healthy() {
			continue
		}
		ents, _ := os.ReadDir(a.sys(s.Path))
		for _, e := range ents {
			if n := e.Name(); e.IsDir() && !strings.HasPrefix(n, ".") && n != "lost+found" && !seen[n] {
				seen[n] = true
				v.Folders = append(v.Folders, n)
			}
		}
	}
	slices.Sort(v.Folders)
	return p
}

// assignBlocked says why the array can't change now ("" when it can).
// Anything wrong stops every change: adding load to a sick array, or a sync
// that records a missing disk's files as deleted, is how data gets lost.
func assignBlocked(op *ReplaceOp, data []ArraySlot, parity []ParitySlot, pools []Pool) string {
	if op.active() {
		return fmt.Sprintf("%s is in progress. One disk job at a time: finish that first.", opTitle(op))
	}
	for _, s := range data {
		if !s.healthy() {
			return fmt.Sprintf("%s isn't working (%s). Get it back or replace it first: changing the array needs every disk working.", s.Pool, strings.ToLower(s.State))
		}
		if s.SmartFailing {
			return fmt.Sprintf("%s is failing SMART. Replace it first.", s.Pool)
		}
	}
	// A parity disk that's down doesn't block everything: replacing it is
	// how it comes back (planAssign blocks adding disks meanwhile).
	for _, p := range pools {
		if p.ScanState == "resilvering" {
			return fmt.Sprintf("%s is resilvering. Wait for it to finish.", p.Name)
		}
	}
	return ""
}

// assignReasons says why a spare disk can't fill each kind of slot. Parity
// has to be at least as big as the biggest data disk.
func assignReasons(c Candidate, data []ArraySlot, parity []ParitySlot) AssignCandidate {
	var maxData, minParity uint64
	var biggest, smallest string
	for _, s := range data {
		if s.Size > maxData {
			maxData, biggest = s.Size, s.Pool
		}
	}
	for _, s := range parity {
		if s.Mounted && s.Size > 0 && (minParity == 0 || s.Size < minParity) {
			minParity, smallest = s.Size, s.Label
		}
	}
	ac := AssignCandidate{Candidate: c, NotSwap: map[string]string{}}
	base := c.Blocked
	if base == "" && c.Transport == "usb" {
		base = "USB disks aren't used in the array: they tend to disconnect under load."
	}
	if base != "" {
		ac.NotData, ac.NotParity = base, base
	} else {
		if minParity > 0 && c.Size > minParity {
			ac.NotData = fmt.Sprintf("Bigger than %s (%s). Parity has to be at least as big as the biggest data disk, so give parity a disk this big first.", smallest, human(minParity))
		}
		if c.Size < maxData {
			ac.NotParity = fmt.Sprintf("Smaller than %s (%s). Parity has to be at least as big as the biggest data disk.", biggest, human(maxData))
		}
	}
	for _, s := range parity {
		switch {
		case base != "":
			ac.NotSwap[s.Name] = base
		case !s.Mounted: // rebuilt from the data disks: only their size matters
			ac.NotSwap[s.Name] = ac.NotParity
		case c.Size < s.Size:
			ac.NotSwap[s.Name] = fmt.Sprintf("Smaller than %s's disk (%s).", s.Label, human(s.Size))
		case ac.NotParity != "":
			ac.NotSwap[s.Name] = ac.NotParity
		case float64(c.Size)*0.98 < float64(s.Used):
			ac.NotSwap[s.Name] = fmt.Sprintf("Too small for %s's parity file (%s).", s.Label, human(s.Used))
		}
	}
	return ac
}

// paritySlots lists the parity levels in snapraid.conf and their disks.
func (a *Agent) paritySlots(ctx context.Context, conf *snapConf, disks []Disk) []ParitySlot {
	out := []ParitySlot{}
	for _, c := range conf.parity {
		s := ParitySlot{Name: c.Name, Level: parityLevel(strings.SplitN(c.Name, "#", 2)[0]), File: c.Path, Mount: parentMount(c.Path)}
		s.Label = fmt.Sprintf("Parity %d", s.Level)
		if s.Level == 0 {
			s.Label = "Parity (" + c.Name + ")"
		}
		if o, err := a.cmd(ctx, 5*time.Second, "findmnt", "-rno", "SOURCE,FSTYPE,UUID", "--mountpoint", s.Mount); err == nil {
			if f := strings.Fields(o); len(f) >= 2 {
				s.Mounted, s.FSType = true, f[1]
				if len(f) >= 3 {
					s.UUID = f[2]
				}
				name := a.parentDisk(ctx, strings.SplitN(f[0], "[", 2)[0])
				for _, d := range disks {
					if d.Name == name {
						s.DevName, s.Model, s.Serial, s.Size, s.Failing = d.Name, d.Model, d.Serial, d.Size, d.Health == "failed"
						s.Device = a.byID(d.Name)
					}
				}
			}
		}
		if !s.Mounted {
			// How it was mounted, and which disk that was if it's still
			// connected, for rebuilding it onto a new one.
			if f := fstabEntry(a.readFstab(), s.Mount); f != nil {
				s.Source, s.FSType = f[0], f[2]
				if u, ok := strings.CutPrefix(f[0], "UUID="); ok {
					s.UUID = u
					if o, err := a.cmd(ctx, 5*time.Second, "blkid", "-U", u); err == nil && strings.TrimSpace(o) != "" {
						name := a.parentDisk(ctx, strings.TrimSpace(o))
						for _, d := range disks {
							if d.Name == name {
								s.DevName, s.Model, s.Serial, s.Size, s.Failing = d.Name, d.Model, d.Serial, d.Size, d.Health == "failed"
								s.Device = a.byID(d.Name)
							}
						}
					}
				}
			}
		} else if fi, err := os.Stat(a.sys(c.Path)); err == nil {
			s.Used = uint64(fi.Size())
		}
		out = append(out, s)
	}
	return out
}

// parityCheck says why a parity slot can't be moved (replace-parity, the
// disk works) or rebuilt (rebuild-parity, it doesn't) right now.
func parityCheck(action string, s *ParitySlot, all []ParitySlot) error {
	if s == nil {
		return refuse("There's no such parity level in snapraid.conf.")
	}
	for _, o := range all {
		if o.Name != s.Name && !o.Mounted {
			return refuse("%s isn't mounted either. One parity disk at a time: get it back or replace it first.", o.Label)
		}
	}
	switch {
	case action == "replace-parity" && !s.Mounted:
		return refuse("%s isn't mounted any more, so its parity can't be copied: it has to be rebuilt instead. Reload the page.", s.Label)
	case action == "rebuild-parity" && s.Mounted:
		return refuse("%s is mounted again, so its parity is copied instead of rebuilt. Reload the page.", s.Label)
	case action == "rebuild-parity" && s.Source == "":
		return refuse("/etc/fstab has no line for %s, so Homestack can't tell how %s was mounted. Set it up by hand.", s.Mount, s.Label)
	case !parityFS[s.FSType]:
		fs := s.FSType
		if fs == "" {
			fs = "an unknown filesystem"
		}
		return refuse("%s's disk is %s. Homestack only sets up ext4 or XFS parity disks.", s.Label, fs)
	case action == "replace-parity" && s.UUID == "":
		return refuse("Couldn't tell which filesystem %s is on (findmnt shows no UUID).", s.Label)
	}
	return nil
}

// parentDisk is the disk a partition is on (sdh1 -> sdh), or the disk itself.
func (a *Agent) parentDisk(ctx context.Context, dev string) string {
	if o, err := a.cmd(ctx, 5*time.Second, "lsblk", "-no", "PKNAME", dev); err == nil {
		if p := strings.TrimSpace(strings.SplitN(o, "\n", 2)[0]); p != "" {
			return p
		}
	}
	return filepath.Base(dev)
}

func (a *Agent) nextData(ctx context.Context, p *assignPlan, pools []Pool) *NextSlot {
	l := a.layout()
	n := 0
	names := map[string]bool{}
	for _, d := range p.conf.data {
		names[d.Name] = true
		n = max(n, l.DiskNum(d.Path))
	}
	if len(p.conf.data) > 0 && n == 0 {
		return &NextSlot{Label: "New disk", Blocked: fmt.Sprintf("The data disks aren't mounted at %sN, so Homestack can't tell what a new one would be called. Set where they are in Settings → Storage, or add it by hand.", l.DiskPrefix)}
	}
	n++
	s := &NextSlot{Name: fmt.Sprintf("d%d", n), Label: fmt.Sprintf("Disk %d", n), Pool: l.DiskPool(n), Path: l.DiskPath(n)}
	block := func(f string, args ...any) {
		if s.Blocked == "" {
			s.Blocked = fmt.Sprintf(f, args...)
		}
	}
	if names[s.Name] {
		block("snapraid.conf already has a disk called %s.", s.Name)
	}
	for _, pl := range pools {
		if pl.Name == s.Pool || pl.Mountpoint == s.Path {
			block("A ZFS pool called %s, or mounted at %s, already exists.", pl.Name, s.Path)
		}
	}
	if a.isMounted(ctx, s.Path) {
		block("Something is already mounted at %s.", s.Path)
	} else if ents, err := os.ReadDir(a.sys(s.Path)); err == nil && len(ents) > 0 {
		block("%s already exists and isn't empty.", s.Path)
	}
	plain := ""
	for _, sl := range p.view.Data {
		switch {
		case sl.healthy() && sl.FSType == "zfs" && p.template == "":
			p.template = sl.Pool
		case sl.plain():
			plain = sl.FSType
		}
	}
	switch {
	case p.template == "" && plain != "":
		block("Homestack adds a data disk as a ZFS pool set up like the others, and these are %s. Add it by hand: format it %s, mount it at %s (fstab), add it to snapraid.conf and the mergerfs line, then sync.", plain, plain, s.Path)
	case p.template == "":
		block("There's no working data pool to copy the settings from.")
	}
	if _, err := a.mergerPlan(ctx, dataPaths(p.conf, ""), s.Path); err != nil {
		block("%s", err)
	}
	for _, t := range []string{"zpool", "wipefs"} {
		if !a.have(ctx, t) {
			block("%s isn't installed.", t)
		}
	}
	if s.Blocked == "" {
		for _, sl := range p.view.Parity {
			if sl.Failing {
				block("%s is failing SMART. Replace it first.", sl.Label)
			}
		}
	}
	return s
}

func (a *Agent) nextParity(ctx context.Context, p *assignPlan) (*NextSlot, string) {
	top := 0
	for _, s := range p.view.Parity {
		switch {
		case strings.Contains(s.Name, "#"):
			return nil, "Parity is split over several files (a comma-separated parity line), which Homestack doesn't change. Change it by hand."
		case s.Level == 0:
			return nil, fmt.Sprintf("snapraid.conf has a parity level called %q, which Homestack doesn't know. Change parity by hand.", s.Name)
		}
		top = max(top, s.Level)
	}
	// A new parity disk is formatted and mounted like the first one.
	p.fsType, p.opts = "ext4", "defaults,nofail"
	if len(p.view.Parity) > 0 {
		first := p.view.Parity[0]
		p.fsType = first.FSType
		if f := fstabEntry(a.readFstab(), first.Mount); len(f) >= 4 {
			p.opts = f[3]
		}
		if !slices.Contains(strings.Split(p.opts, ","), "nofail") {
			p.opts += ",nofail"
		}
	}
	if !parityFS[p.fsType] {
		return nil, fmt.Sprintf("Parity 1 is %s. Homestack only sets up ext4 or XFS parity disks, so change parity by hand.", p.fsType)
	}
	for _, t := range []string{"parted", "partprobe", "blkid", "wipefs", "mkfs." + p.fsType} {
		if !a.have(ctx, t) {
			pkg := map[string]string{"parted": "parted", "partprobe": "parted", "mkfs.xfs": "xfsprogs", "mkfs.ext4": "e2fsprogs"}[t]
			if pkg == "" {
				pkg = "util-linux"
			}
			return nil, fmt.Sprintf("%s isn't installed. Install it first: sudo apt install %s", t, pkg)
		}
	}
	l := top + 1
	s := &NextSlot{Name: parityName(l), Label: fmt.Sprintf("Parity %d", l), Level: l, Path: a.layout().ParityPath(l)}
	s.File = s.Path + "/" + parityFileName(l)
	switch {
	case l > maxParity:
		s.Blocked = "SnapRAID has 6 parity levels at most."
	case a.isMounted(ctx, s.Path):
		s.Blocked = fmt.Sprintf("Something is already mounted at %s.", s.Path)
	case fstabEntry(a.readFstab(), s.Path) != nil:
		s.Blocked = fmt.Sprintf("/etc/fstab already has a line for %s.", s.Path)
	}
	if ents, err := os.ReadDir(a.sys(s.Path)); s.Blocked == "" && err == nil && len(ents) > 0 {
		s.Blocked = fmt.Sprintf("%s already exists and isn't empty.", s.Path)
	}
	for _, sl := range p.view.Parity {
		if sl.Failing && s.Blocked == "" {
			s.Blocked = fmt.Sprintf("%s is failing SMART. Replace it first.", sl.Label)
		}
	}
	return s, ""
}

var reLevel = regexp.MustCompile(`^([1-6])-parity$`)

// parityLevel reads a parity line's keyword: parity is 1, 2-parity is 2.
func parityLevel(name string) int {
	switch name {
	case "parity":
		return 1
	case "q-parity": // the old name for the second
		return 2
	case "z-parity": // an alternative third
		return 3
	}
	if m := reLevel.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func parityName(level int) string {
	if level == 1 {
		return "parity"
	}
	return fmt.Sprintf("%d-parity", level)
}

func parityFileName(level int) string { return "snapraid." + parityName(level) }

// dataPaths are the data disks' mount points, without skip.
func dataPaths(conf *snapConf, skip string) []string {
	var out []string
	for _, d := range conf.data {
		if p := strings.TrimRight(d.Path, "/"); p != skip {
			out = append(out, p)
		}
	}
	return out
}

// have says whether a program is installed.
func (a *Agent) have(ctx context.Context, name string) bool {
	_, err := a.cmd(ctx, 5*time.Second, "which", name)
	return err == nil
}

// mountedUUID is the UUID of what's mounted at path ("" when nothing is).
func (a *Agent) mountedUUID(ctx context.Context, path string) string {
	o, err := a.cmd(ctx, 5*time.Second, "findmnt", "-rno", "UUID", "--mountpoint", path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(o)
}

// ---------- Starting ----------

type assignReq struct {
	Action  string `json:"action"` // add-data, add-parity, replace-parity
	Device  string `json:"device"`
	Confirm string `json:"confirm"` // the disk's serial
	Parity  string `json:"parity,omitempty"`
	Folders bool   `json:"folders"` // add-data: create the share folders on it
	Verify  bool   `json:"verify"`  // rebuild-parity: check the rebuilt parity afterwards
}

func (a *Agent) assignStart(ctx context.Context, req assignReq) error {
	if err := a.notBusy(ctx); err != nil {
		return err
	}
	p := a.planAssign(ctx)
	v := p.view
	if v.Blocked != "" {
		return refuse("%s", v.Blocked)
	}
	var c *AssignCandidate
	for i := range v.Candidates {
		if req.Device != "" && v.Candidates[i].Device == req.Device {
			c = &v.Candidates[i]
		}
	}
	if c == nil {
		return refuse("That disk isn't available any more. Reload the page.")
	}
	op := &ReplaceOp{
		ID: newID(), Method: req.Action, NewDevice: c.Device, NewModel: c.Model, NewSerial: c.Serial, NewSize: c.Size,
		State: "running", Started: time.Now(),
	}
	switch req.Action {
	case "add-data":
		s := v.NextData
		switch {
		case s == nil:
			return refuse("A data disk can't be added right now.")
		case s.Blocked != "":
			return refuse("A data disk can't be added: %s", s.Blocked)
		case c.NotData != "":
			return refuse("%s", c.NotData)
		}
		// Slow checks, left for the start.
		if a.importablePools(ctx)[s.Pool] {
			return refuse("A disk with a pool called %s is connected but not imported. Import or wipe it first, so there aren't two.", s.Pool)
		}
		args, err := a.templateArgs(ctx, p.template)
		if err != nil {
			return err
		}
		op.Snap, op.Pool, op.Path, op.Template, op.CreateArgs = s.Name, s.Pool, s.Path, p.template, args
		if req.Folders {
			op.Folders = v.Folders
		}
	case "add-parity":
		s := v.NextParity
		switch {
		case v.ParityBlocked != "":
			return refuse("%s", v.ParityBlocked)
		case s == nil:
			return refuse("A parity disk can't be added right now.")
		case s.Blocked != "":
			return refuse("A parity disk can't be added: %s", s.Blocked)
		case c.NotParity != "":
			return refuse("%s", c.NotParity)
		}
		op.Snap, op.Pool, op.Path, op.ParityFile, op.Level = s.Name, strings.ToLower(s.Label), s.Path, s.File, s.Level
		op.FSType, op.MountOpts = p.fsType, p.opts
	case "replace-parity", "rebuild-parity":
		var s *ParitySlot
		for i := range v.Parity {
			if v.Parity[i].Name == req.Parity {
				s = &v.Parity[i]
			}
		}
		if v.ParityBlocked != "" {
			return refuse("%s", v.ParityBlocked)
		}
		if err := parityCheck(req.Action, s, v.Parity); err != nil {
			return err
		}
		if c.NotSwap[s.Name] != "" {
			return refuse("%s", c.NotSwap[s.Name])
		}
		if !a.have(ctx, "mkfs."+s.FSType) {
			return refuse("mkfs.%s isn't installed.", s.FSType)
		}
		op.Snap, op.Pool, op.Path, op.ParityFile, op.Level = s.Name, strings.ToLower(s.Label), s.Mount, s.File, s.Level
		op.FSType, op.OldUUID, op.Used = s.FSType, s.UUID, s.Used
		op.OldDevice, op.OldModel, op.OldSerial, op.OldSize = s.Device, s.Model, s.Serial, s.Size
		if req.Action == "replace-parity" {
			op.TempMount = s.Mount + "-new"
		} else {
			op.Verify = req.Verify
		}
	default:
		return refuse("Unknown action.")
	}
	if strings.TrimSpace(req.Confirm) != c.Serial {
		return refuse("Type the disk's serial number (%s) to confirm.", c.Serial)
	}
	op.Steps = assignSteps(op)
	a.saveOp(op)
	a.notifyHost(notify.Warning, opTitle(op), startMessage(op))
	return a.runReplaceJob()
}

// assignSteps are the steps of an add or a parity move.
func assignSteps(op *ReplaceOp) []ReplaceStep {
	mk := func(k, t string) ReplaceStep { return ReplaceStep{Key: k, Title: t, State: "pending"} }
	hold, release := mk("hold", "Pause the mover and parity jobs"), mk("release", "Resume the mover and parity jobs")
	wipe, format := mk("wipe", "Prepare the new disk"), mk("format", "Partition and format it ("+op.FSType+")")
	switch op.Method {
	case "add-data":
		folders := mk("folders", "Create the share folders on it")
		if len(op.Folders) == 0 {
			folders.Title = "Set its owner and permissions like the other disks'"
		}
		return []ReplaceStep{
			hold, wipe,
			mk("create", "Create "+op.Pool+" on it"),
			folders,
			mk("config", "Add it to SnapRAID as "+op.Snap),
			mk("mergerfs", "Add it to the shares (mergerfs)"),
			mk("sync", "Update parity"),
			release,
		}
	case "add-parity":
		return []ReplaceStep{
			hold, wipe, format,
			mk("mount", "Mount it at "+op.Path),
			mk("config", "Add it to SnapRAID as "+op.Snap),
			mk("sync", "Build the new parity (reads every disk)"),
			release,
		}
	case "replace-parity":
		return []ReplaceStep{
			hold, wipe, format,
			mk("copy", "Copy the parity onto it"),
			mk("swap", "Put it in place of the old disk"),
			mk("sync", "Check parity is up to date"),
			release,
		}
	case "rebuild-parity":
		verify := mk("verify", "Check the rebuilt parity (reads every disk again)")
		if !op.Verify {
			verify.State, verify.Detail = "skipped", "Not chosen."
		}
		return []ReplaceStep{
			hold, wipe, format,
			mk("place", "Put it in place of the old disk"),
			mk("fix", "Rebuild the parity from the data disks"),
			verify,
			mk("sync", "Update parity"),
			release,
		}
	}
	return nil
}

// opTitle names a disk job: "Replacing disk3", "Adding disk6".
func opTitle(op *ReplaceOp) string {
	if strings.HasPrefix(op.Method, "add-") {
		return "Adding " + op.Pool
	}
	return "Replacing " + op.Pool
}

func startMessage(op *ReplaceOp) string {
	disk := fmt.Sprintf("%s (%s)", op.NewModel, op.NewSerial)
	switch op.Method {
	case "add-data":
		return fmt.Sprintf("%s is being added to the array as %s. The mover and parity jobs are paused until it's done.", disk, op.Pool)
	case "add-parity":
		return fmt.Sprintf("%s is being added as %s. Building it reads every disk, so it takes a while (about a day for big disks). The mover and parity jobs are paused until it's done.", disk, op.Pool)
	case "replace-parity":
		return fmt.Sprintf("%s is moving to %s. Copying it takes a while. The mover and parity jobs are paused until it's done.", op.Pool, disk)
	case "rebuild-parity":
		return fmt.Sprintf("%s is being rebuilt from the data disks onto %s, which reads every disk (about a day for big disks). Until it's done, a data disk failing could lose its files. The mover and parity jobs are paused until it's done.", op.Pool, disk)
	}
	how := map[string]string{"rebuild": "rebuilt from parity", "copy": "copied"}[op.Method]
	return fmt.Sprintf("%s is being %s onto %s. The mover and parity jobs are paused until it's done.", op.Pool, how, disk)
}

func doneMessage(op *ReplaceOp, took time.Duration) (title, msg string) {
	disk := fmt.Sprintf("%s (%s)", op.NewModel, op.NewSerial)
	switch op.Method {
	case "add-data":
		return op.Pool + " added", fmt.Sprintf("%s is in the array as %s: in SnapRAID as %s and in the shares. Took %s.", disk, op.Pool, op.Snap, humanDur(took))
	case "add-parity":
		return op.Pool + " added", fmt.Sprintf("%s is on %s: up to %d disks can now fail without losing data. Took %s.", op.Pool, disk, op.Level, humanDur(took))
	case "replace-parity":
		return op.Pool + " replaced", fmt.Sprintf("%s is now on %s. The old disk (%s) isn't used any more and keeps a copy of the parity until you reuse or remove it. Took %s.", op.Pool, disk, op.OldSerial, humanDur(took))
	case "rebuild-parity":
		return op.Pool + " replaced", fmt.Sprintf("%s is rebuilt on %s, and the array is protected again. Took %s.", op.Pool, disk, humanDur(took))
	}
	return op.Pool + " replaced", fmt.Sprintf("%s is now on %s. Took %s.", op.Pool, disk, humanDur(took))
}

// ---------- The steps ----------

// stepFormat gives a new parity disk one partition and a filesystem.
func (a *Agent) stepFormat(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	name, err := a.newDisk(op)
	if err != nil {
		return "", err
	}
	part := op.NewDevice + "-part1"
	if op.NewUUID != "" && a.blkid(ctx, part) == op.NewUUID {
		return "Already formatted.", nil
	}
	if role := a.roles(ctx)[name]; role != "" {
		return "", fmt.Errorf("the new disk is in use as %s", role)
	}
	label := strings.ReplaceAll(op.Pool, " ", "") // parity2
	args := []string{"-s", "-a", "optimal", op.NewDevice, "mklabel", "gpt", "mkpart", label, op.FSType, "0%", "100%"}
	log("  parted " + strings.Join(args, " "))
	if out, err := a.cmd(ctx, 2*time.Minute, "parted", args...); err != nil {
		return "", fmt.Errorf("parted: %s", strings.TrimSpace(out))
	}
	_, _ = a.cmd(ctx, 30*time.Second, "partprobe", op.NewDevice)
	_, _ = a.cmd(ctx, 30*time.Second, "udevadm", "settle")
	var mkfs []string
	switch op.FSType {
	case "ext4": // no reserved blocks, few inodes: it holds one huge file
		mkfs = []string{"mkfs.ext4", "-F", "-q", "-m", "0", "-T", "largefile4", "-L", label, part}
	case "xfs":
		mkfs = []string{"mkfs.xfs", "-f", "-q", "-L", label, part}
	default:
		return "", fmt.Errorf("can't format %s", op.FSType)
	}
	log("  " + strings.Join(mkfs, " "))
	if out, err := a.cmd(ctx, 30*time.Minute, mkfs[0], mkfs[1:]...); err != nil {
		return "", fmt.Errorf("%s: %s", mkfs[0], strings.TrimSpace(out))
	}
	_, _ = a.cmd(ctx, 30*time.Second, "udevadm", "settle")
	if op.NewUUID = a.blkid(ctx, part); op.NewUUID == "" {
		return "", errors.New("the new filesystem has no UUID (blkid)")
	}
	a.saveOp(op)
	return fmt.Sprintf("One partition, %s, labelled %s (UUID %s).", op.FSType, label, op.NewUUID), nil
}

func (a *Agent) blkid(ctx context.Context, dev string) string {
	o, err := a.cmd(ctx, 10*time.Second, "blkid", "-s", "UUID", "-o", "value", dev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(o)
}

// stepMountParity mounts a new parity disk for good (/etc/fstab).
func (a *Agent) stepMountParity(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	switch u := a.mountedUUID(ctx, op.Path); u {
	case op.NewUUID:
		return "Already mounted.", nil
	case "":
	default:
		return "", fmt.Errorf("another disk is mounted at %s", op.Path)
	}
	dir := a.sys(op.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if ents, _ := os.ReadDir(dir); len(ents) > 0 {
		return "", fmt.Errorf("%s isn't empty", op.Path)
	}
	err := a.editFstab(ctx, op, func(lines []string) ([]string, error) {
		for _, l := range lines {
			if f := fstabFields(l); f != nil && f[1] == op.Path {
				if f[0] == "UUID="+op.NewUUID {
					return nil, nil // there from an earlier try
				}
				return nil, fmt.Errorf("/etc/fstab already mounts %s at %s", f[0], op.Path)
			}
		}
		pass := "0"
		if op.FSType == "ext4" {
			pass = "2"
		}
		return append(lines,
			fmt.Sprintf("# %s: %s %s, added by Homestack on %s", op.Pool, op.NewModel, op.NewSerial, time.Now().Format("2 Jan 2006")),
			fmt.Sprintf("UUID=%s  %s  %s  %s  0  %s", op.NewUUID, op.Path, op.FSType, op.MountOpts, pass)), nil
	})
	if err != nil {
		return "", err
	}
	if out, err := a.cmd(ctx, time.Minute, "mount", op.Path); err != nil {
		return "", fmt.Errorf("mount %s: %s", op.Path, strings.TrimSpace(out))
	}
	if a.mountedUUID(ctx, op.Path) != op.NewUUID {
		return "", fmt.Errorf("%s didn't mount the new disk", op.Path)
	}
	return fmt.Sprintf("Mounted at %s and added to /etc/fstab (nofail: a missing disk can't hold up a boot).", op.Path), nil
}

// stepFolders gives a new data disk the share folders, owned like on the
// other disks, so mergerfs's "existing path" policies can use it.
func (a *Agent) stepFolders(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	if !a.isMounted(ctx, op.Path) {
		return "", fmt.Errorf("%s isn't mounted", op.Path)
	}
	conf, err := readSnapConf(a.snapraidConf())
	if err != nil {
		return "", err
	}
	others := dataPaths(conf, op.Path)
	if len(others) == 0 {
		return "No other data disk to copy from.", nil
	}
	ref := ""
	for _, o := range others {
		if _, err := os.Stat(a.sys(o)); err == nil {
			ref = o
			break
		}
	}
	if ref == "" {
		return "", errors.New("none of the other data disks is there to copy from")
	}
	if err := copyOwner(a.sys(ref), a.sys(op.Path)); err != nil {
		return "", err
	}
	for _, f := range op.Folders {
		src := ""
		for _, o := range others {
			if fi, err := os.Stat(filepath.Join(a.sys(o), f)); err == nil && fi.IsDir() {
				src = filepath.Join(a.sys(o), f)
				break
			}
		}
		dst := filepath.Join(a.sys(op.Path), f)
		if src == "" { // gone meanwhile
			continue
		}
		if err := os.Mkdir(dst, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err := copyOwner(src, dst); err != nil {
			return "", err
		}
	}
	if len(op.Folders) == 0 {
		return fmt.Sprintf("Owner and permissions as on %s.", filepath.Base(ref)), nil
	}
	return fmt.Sprintf("%d folder%s (%s), owned as on the other disks.", len(op.Folders), plural(len(op.Folders)), strings.Join(op.Folders, ", ")), nil
}

// copyOwner gives dst src's owner, group and mode.
func copyOwner(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(dst, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	return os.Chmod(dst, fi.Mode().Perm()|fi.Mode()&(os.ModeSetgid|os.ModeSticky))
}

// stepConfig puts the new disk into snapraid.conf.
func (a *Agent) stepConfig(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	path := a.snapraidConf()
	conf, err := readSnapConf(path)
	if err != nil {
		return "", err
	}
	var line, kind string
	switch op.Method {
	case "add-data":
		for _, d := range conf.data {
			switch p := strings.TrimRight(d.Path, "/"); {
			case d.Name == op.Snap && p == op.Path:
				return "Already in snapraid.conf.", nil
			case d.Name == op.Snap:
				return "", fmt.Errorf("snapraid.conf already has a %s, at %s", op.Snap, d.Path)
			case p == op.Path:
				return "", fmt.Errorf("snapraid.conf already has %s, as %s", op.Path, d.Name)
			}
		}
		// Written like the lines there: "data" or the older "disk", and
		// with or without the trailing slash.
		kw, slash := "data", "/"
		for l := range strings.SplitSeq(readText(path), "\n") {
			if f := strings.Fields(l); len(f) >= 3 && (f[0] == "data" || f[0] == "disk") {
				kw = f[0]
				slash = map[bool]string{true: "/", false: ""}[strings.HasSuffix(f[2], "/")]
			}
		}
		line, kind = fmt.Sprintf("%s %s %s%s", kw, op.Snap, op.Path, slash), "data"
	case "add-parity":
		for _, p := range conf.parity {
			switch {
			case p.Path == op.ParityFile:
				return "Already in snapraid.conf.", nil
			case strings.SplitN(p.Name, "#", 2)[0] == op.Snap:
				return "", fmt.Errorf("snapraid.conf already has a %s, at %s", op.Snap, p.Path)
			}
		}
		line, kind = op.Snap+" "+op.ParityFile, "parity"
	default:
		return "", fmt.Errorf("nothing to add for %s", op.Method)
	}
	if err := a.addSnapLine(op, kind, line); err != nil {
		return "", err
	}
	log("  " + line)
	return fmt.Sprintf("Added `%s` (a copy of the old file is kept next to it).", line), nil
}

// stepMergerfs adds a new data disk to the mergerfs views of the data
// disks: in /etc/fstab first, so it stays after a restart, then live.
func (a *Agent) stepMergerfs(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	conf, err := readSnapConf(a.snapraidConf())
	if err != nil {
		return "", err
	}
	changes, err := a.mergerPlan(ctx, dataPaths(conf, op.Path), op.Path)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "There's no mergerfs view of the data disks to add it to.", nil
	}
	err = a.editFstab(ctx, op, func(lines []string) ([]string, error) {
		changed := false
		for i, l := range lines {
			f := fstabFields(l)
			for _, c := range changes {
				if f != nil && c.NewSrc != "" && f[1] == c.Target && f[0] == c.OldSrc {
					lines[i], changed = replaceField(l, 0, c.NewSrc), true
				}
			}
		}
		if !changed {
			return nil, nil
		}
		return lines, nil
	})
	if err != nil {
		return "", err
	}
	var targets []string
	for _, c := range changes {
		targets = append(targets, c.Target)
		if !c.Live {
			continue
		}
		ctl := filepath.Join(a.sys(c.Target), ".mergerfs")
		log("  " + c.Target + ": +>" + op.Path)
		if err := a.xattr.set(ctl, "user.mergerfs.branches", "+>"+op.Path); err != nil {
			return "", fmt.Errorf("couldn't add %s to %s: %v", op.Path, c.Target, err)
		}
		if raw, err := a.xattr.get(ctl, "user.mergerfs.branches"); err != nil || !slices.Contains(splitBranches(raw), op.Path) {
			return "", fmt.Errorf("%s didn't take %s", c.Target, op.Path)
		}
	}
	return fmt.Sprintf("In %s now, and in /etc/fstab for after a restart.", strings.Join(targets, " and ")), nil
}

// stepCopyParity copies the files on the old parity disk to the new one,
// mounted next to it. It carries on where it stopped.
func (a *Agent) stepCopyParity(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	if a.mountedUUID(ctx, op.Path) != op.OldUUID {
		return "", fmt.Errorf("the old parity disk isn't mounted at %s", op.Path)
	}
	if err := a.mountTemp(ctx, op); err != nil {
		return "", err
	}
	files, total, err := parityFiles(a.sys(op.Path))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("there's nothing on %s to copy", op.Path)
	}
	dst := a.sys(op.TempMount)
	var have uint64 // already copied, from an earlier try
	for _, f := range files {
		if fi, err := os.Stat(filepath.Join(dst, f)); err == nil {
			have += uint64(fi.Size())
		} else if fi, err := os.Stat(filepath.Join(dst, f) + partSuffix); err == nil {
			have += uint64(fi.Size())
		}
	}
	var sfs syscall.Statfs_t
	if err := syscall.Statfs(dst, &sfs); err == nil {
		if free := sfs.Bavail * uint64(sfs.Bsize); free+have < total {
			return "", fmt.Errorf("the new disk has %s free, not enough for %s of parity", human(free), human(total))
		}
	}
	defer a.jobs.byteProgress(0, 0, time.Time{}, 0)
	var done uint64
	since, from, lastLog := time.Now(), uint64(0), time.Now()
	for i, f := range files {
		base := done
		err := copyResumable(ctx, filepath.Join(a.sys(op.Path), f), filepath.Join(dst, f), func(n int64) {
			at := base + uint64(n)
			if from == 0 && at > 0 {
				since, from = time.Now(), at // count the speed from where it resumed
			}
			a.jobs.byteProgress(at, total, since, from)
			if time.Since(lastLog) > 5*time.Minute {
				lastLog = time.Now()
				log(fmt.Sprintf("  %s of %s copied", human(at), human(total)))
			}
		})
		if err != nil {
			if errors.Is(err, errStopped) || ctx.Err() != nil {
				return "", errStopped
			}
			return "", fmt.Errorf("copying %s: %v. If the old parity disk is failing, cancel, unmount it (sudo umount -l %s) and replace it again: it's then rebuilt from the data disks instead", f, err, op.Path)
		}
		fi, _ := os.Stat(filepath.Join(a.sys(op.Path), f))
		done += uint64(fi.Size())
		log(fmt.Sprintf("  %s copied (%d of %d)", f, i+1, len(files)))
	}
	return fmt.Sprintf("Copied %d file%s, %s.", len(files), plural(len(files)), human(total)), nil
}

func (a *Agent) mountTemp(ctx context.Context, op *ReplaceOp) error {
	switch u := a.mountedUUID(ctx, op.TempMount); u {
	case op.NewUUID:
		return nil
	case "":
	default:
		return fmt.Errorf("another disk is mounted at %s", op.TempMount)
	}
	if err := os.MkdirAll(a.sys(op.TempMount), 0o755); err != nil {
		return err
	}
	if out, err := a.cmd(ctx, time.Minute, "mount", "-t", op.FSType, "UUID="+op.NewUUID, op.TempMount); err != nil {
		return fmt.Errorf("mounting the new disk: %s", strings.TrimSpace(out))
	}
	if a.mountedUUID(ctx, op.TempMount) != op.NewUUID {
		return fmt.Errorf("%s didn't mount the new disk", op.TempMount)
	}
	return nil
}

// parityFiles are the files at the top of a parity disk (the parity file,
// and a content file if one is kept there), with their total size.
func parityFiles(dir string) ([]string, uint64, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var out []string
	var total uint64
	for _, e := range ents {
		if !e.Type().IsRegular() || strings.Contains(e.Name(), partSuffix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e.Name())
		total += uint64(fi.Size())
	}
	return out, total, nil
}

const partSuffix = ".homestack-part"

// copyResumable copies src to dst through dst.homestack-part, and carries on
// where an earlier try left off. Progress is saved (as an offset in
// dst.homestack-part.ok) only once the data before it is on the disk.
func copyResumable(ctx context.Context, src, dst string, progress func(int64)) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if di, err := os.Stat(dst); err == nil && di.Size() == si.Size() && di.ModTime().Equal(si.ModTime()) {
		progress(si.Size())
		return nil // copied by an earlier try
	}
	part, okFile := dst+partSuffix, dst+partSuffix+".ok"
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	var off int64
	if b, err := os.ReadFile(okFile); err == nil {
		off, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	if pi, err := out.Stat(); err != nil || off < 0 || off > pi.Size() || off > si.Size() {
		off = 0
	}
	if _, err := in.Seek(off, io.SeekStart); err != nil {
		return err
	}
	if _, err := out.Seek(off, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 4<<20)
	done, synced := off, off
	progress(done)
	for {
		if ctx.Err() != nil {
			if out.Sync() == nil {
				_ = os.WriteFile(okFile, []byte(strconv.FormatInt(done, 10)), 0o600)
			}
			return errStopped
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
			done += int64(n)
			progress(done)
			if done-synced >= 1<<30 { // a checkpoint every GB
				if err := out.Sync(); err != nil {
					return err
				}
				synced = done
				_ = os.WriteFile(okFile, []byte(strconv.FormatInt(done, 10)), 0o600)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if done != si.Size() {
		return fmt.Errorf("the copy stopped at %d of %d bytes", done, si.Size())
	}
	if err := out.Truncate(done); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = copyOwner(src, part)
	if err := os.Chtimes(part, si.ModTime(), si.ModTime()); err != nil {
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	_ = os.Remove(okFile)
	return nil
}

// stepSwapParity puts the new parity disk where the old one was mounted,
// and its UUID in the old one's /etc/fstab line.
func (a *Agent) stepSwapParity(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	if a.mountedUUID(ctx, op.Path) != op.NewUUID {
		// Both copies have to match before the old one goes.
		if a.mountedUUID(ctx, op.Path) == op.OldUUID {
			if err := a.mountTemp(ctx, op); err != nil {
				return "", err
			}
			if err := sameFiles(a.sys(op.Path), a.sys(op.TempMount)); err != nil {
				return "", fmt.Errorf("the copy doesn't match the old disk (%v); it needs copying again", err)
			}
		}
		if a.mountedUUID(ctx, op.TempMount) != "" {
			if out, err := a.cmd(ctx, time.Minute, "umount", op.TempMount); err != nil {
				return "", fmt.Errorf("umount %s: %s", op.TempMount, strings.TrimSpace(out))
			}
		}
		if a.mountedUUID(ctx, op.Path) == op.OldUUID {
			log("  umount " + op.Path)
			if out, err := a.cmd(ctx, time.Minute, "umount", op.Path); err != nil {
				return "", fmt.Errorf("the old parity disk is busy (umount: %s). Close what has %s open, then Continue", strings.TrimSpace(out), op.Path)
			}
		}
		if err := a.mountInPlace(ctx, op, log); err != nil {
			return "", err
		}
	}
	files, _, err := parityFiles(a.sys(op.Path))
	if err != nil || !slices.Contains(files, filepath.Base(op.ParityFile)) {
		return "", fmt.Errorf("%s isn't on the new disk", filepath.Base(op.ParityFile))
	}
	_ = os.Remove(a.sys(op.TempMount))
	return fmt.Sprintf("%s is mounted from the new disk. The old one (%s) is unmounted, with its copy kept.", op.Path, op.OldSerial), nil
}

// mountInPlace mounts the new parity disk at op.Path through the path's
// /etc/fstab line, given the new disk's UUID. If it won't mount, the line
// goes back to what it was before the job (and that disk is mounted again,
// when it works), so parity isn't left pointing at nothing.
func (a *Agent) mountInPlace(ctx context.Context, op *ReplaceOp, log func(string)) error {
	err := a.editFstab(ctx, op, func(lines []string) ([]string, error) {
		for i, l := range lines {
			if f := fstabFields(l); f != nil && f[1] == op.Path {
				if f[0] == "UUID="+op.NewUUID {
					return nil, nil // done by an earlier try
				}
				lines[i] = replaceField(replaceField(l, 2, op.FSType), 0, "UUID="+op.NewUUID)
				return lines, nil
			}
		}
		return nil, fmt.Errorf("/etc/fstab has no line for %s", op.Path)
	})
	if err != nil {
		return err
	}
	log("  mount " + op.Path)
	out, err := a.cmd(ctx, time.Minute, "mount", op.Path)
	if err == nil && a.mountedUUID(ctx, op.Path) == op.NewUUID {
		return nil
	}
	if orig := fstabLine(readText(a.fstabPath()+".homestack-"+op.ID), op.Path); orig != "" {
		_ = a.editFstab(ctx, op, func(lines []string) ([]string, error) {
			for i, l := range lines {
				if f := fstabFields(l); f != nil && f[1] == op.Path {
					lines[i] = orig
				}
			}
			return lines, nil
		})
		_, _ = a.cmd(ctx, time.Minute, "mount", op.Path) // fails harmlessly when that disk is dead
	}
	return fmt.Errorf("the new disk didn't mount at %s (%s), so /etc/fstab is back as it was", op.Path, strings.TrimSpace(out))
}

// stepPlaceParity puts the new disk where a parity disk that stopped
// working was mounted, through its /etc/fstab line.
func (a *Agent) stepPlaceParity(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	switch u := a.mountedUUID(ctx, op.Path); {
	case u == op.NewUUID:
		return "Already in place.", nil
	case u != "":
		return "", fmt.Errorf("a disk is mounted at %s again (UUID %s). If the old parity disk is back, cancel this and keep it", op.Path, u)
	}
	if err := os.MkdirAll(a.sys(op.Path), 0o755); err != nil {
		return "", err
	}
	if ents, _ := os.ReadDir(a.sys(op.Path)); len(ents) > 0 {
		return "", fmt.Errorf("%s isn't empty, though nothing is mounted there", op.Path)
	}
	if err := a.mountInPlace(ctx, op, log); err != nil {
		return "", err
	}
	return fmt.Sprintf("Mounted at %s in place of the old disk (its /etc/fstab line now has the new disk's UUID).", op.Path), nil
}

// stepFixParity rebuilds a lost parity disk from the data disks, as
// SnapRAID's manual says: snapraid -d parity fix.
func (a *Agent) stepFixParity(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	if a.mountedUUID(ctx, op.Path) != op.NewUUID {
		return "", fmt.Errorf("the new disk isn't mounted at %s", op.Path)
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
	if m := reUnrecoverable.FindStringSubmatch(text); m != nil && m[1] != "0" {
		return "", errAttention{fmt.Sprintf("%s unrecoverable error%s: a data disk couldn't be read in places, so part of the parity couldn't be rebuilt. Check the data disks (SMART, cables), then press Try again, or accept to carry on.", m[1], plural(atoi(m[1])))}
	}
	if rc != 0 || hasErrors(text) {
		return "", fmt.Errorf("snapraid fix exited with code %d: %s", rc, lastLines(out, 3))
	}
	if _, err := os.Stat(a.sys(op.ParityFile)); err != nil {
		return "", fmt.Errorf("SnapRAID didn't write %s", op.ParityFile)
	}
	return "Parity rebuilt from the data disks.", nil
}

// stepVerifyParity checks the rebuilt parity against the data (optional:
// it reads every disk again).
func (a *Agent) stepVerifyParity(ctx context.Context, op *ReplaceOp, log func(string)) (string, error) {
	out := a.snapLog("check")
	rc, err := a.runSnapraid(ctx, out, "-d", op.Snap, "check")
	if err != nil {
		return "", err
	}
	log(indent(tailSnapLog(out, 8)))
	if a.jobs.stopWanted() {
		return "", errStopped
	}
	text := readTail(out, 64<<10)
	if rc == 0 && strings.Contains(text, "Everything OK") {
		return "The rebuilt parity matches the data.", nil
	}
	n := "Some"
	if m := reErrorsTotal.FindStringSubmatch(text); m != nil {
		n = m[1]
	}
	return "", errAttention{fmt.Sprintf("%s error(s) in the rebuilt parity, which points at a disk or memory problem. Check the disks' SMART, then press Try again, or accept to finish (the scheduled parity check looks again).", n)}
}

// fstabLine is the line that mounts target, as written ("" if none).
func fstabLine(text, target string) string {
	for l := range strings.SplitSeq(text, "\n") {
		if f := fstabFields(l); f != nil && f[1] == target {
			return l
		}
	}
	return ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// sameFiles checks two parity folders hold the same files, at the same sizes.
func sameFiles(a, b string) error {
	fa, _, err := parityFiles(a)
	if err != nil {
		return err
	}
	for _, f := range fa {
		x, err1 := os.Stat(filepath.Join(a, f))
		y, err2 := os.Stat(filepath.Join(b, f))
		if err1 != nil || err2 != nil {
			return fmt.Errorf("%s is missing", f)
		}
		if x.Size() != y.Size() {
			return fmt.Errorf("%s is %d bytes, not %d", f, y.Size(), x.Size())
		}
	}
	return nil
}

// ---------- Cancelling ----------

// undoAssign takes back what an add has done so far, or says why it can't.
// A parity move is only undone before the swap.
func (a *Agent) undoAssign(ctx context.Context, op *ReplaceOp) (string, error) {
	switch op.Method {
	case "add-data":
		p, err := a.poolByName(ctx, op.Pool)
		if err != nil {
			return "Nothing was changed on the array.", nil
		}
		name := a.devName(op.NewDevice)
		if name == "" || !a.inPool(ctx, op.Pool, name) {
			return "", refuse("The pool %s isn't on the new disk, so it's left alone. Check it by hand.", op.Pool)
		}
		// Never destroy a pool that might hold something: look inside first.
		if !p.Mounted {
			if out, err := a.cmd(ctx, time.Minute, "zfs", "mount", op.Pool); err != nil || !a.isMounted(ctx, op.Path) {
				return "", refuse("%s isn't mounted, so Homestack can't check it's empty before taking it out (zfs mount: %s). Continue instead.", op.Pool, strings.TrimSpace(out))
			}
		}
		if f := filesOn(a.sys(op.Path), op.Folders); f != "" {
			return "", refuse("%s already holds files (%s), so it stays in the array. Continue instead.", op.Pool, f)
		}
		if err := a.mergerRemove(ctx, op); err != nil {
			return "", err
		}
		if err := a.removeSnapLine(op, func(f []string) bool {
			return len(f) >= 3 && (f[0] == "data" || f[0] == "disk") && f[1] == op.Snap && strings.TrimRight(f[2], "/") == op.Path
		}); err != nil {
			return "", err
		}
		if out, err := a.cmd(ctx, 2*time.Minute, "zpool", "destroy", op.Pool); err != nil {
			return "", fmt.Errorf("zpool destroy %s: %s", op.Pool, strings.TrimSpace(out))
		}
		return fmt.Sprintf("Undone: %s is out of the shares and SnapRAID again, and its pool is destroyed. The disk is empty and unassigned.", op.Pool), nil
	case "add-parity":
		if err := a.removeSnapLine(op, func(f []string) bool { return len(f) >= 2 && f[0] == op.Snap && f[1] == op.ParityFile }); err != nil {
			return "", err
		}
		if op.NewUUID != "" && a.mountedUUID(ctx, op.Path) == op.NewUUID {
			if out, err := a.cmd(ctx, time.Minute, "umount", op.Path); err != nil {
				return "", fmt.Errorf("umount %s: %s", op.Path, strings.TrimSpace(out))
			}
		}
		if op.NewUUID != "" {
			if err := a.editFstab(ctx, op, func(lines []string) ([]string, error) {
				var out []string
				for _, l := range lines {
					if f := fstabFields(l); f != nil && f[1] == op.Path && f[0] == "UUID="+op.NewUUID {
						if n := len(out); n > 0 && strings.HasPrefix(out[n-1], "# "+op.Pool+": ") {
							out = out[:n-1]
						}
						continue
					}
					out = append(out, l)
				}
				return out, nil
			}); err != nil {
				return "", err
			}
		}
		_ = os.Remove(a.sys(op.Path)) // only goes if empty
		return fmt.Sprintf("Undone: %s is out of snapraid.conf and /etc/fstab again. The disk keeps its empty filesystem and is unassigned.", op.Pool), nil
	case "rebuild-parity":
		// Once the new disk has taken over, the only way back to having
		// parity is finishing the rebuild.
		f := fstabEntry(a.readFstab(), op.Path)
		if op.stepDone("place") || (op.NewUUID != "" && (a.mountedUUID(ctx, op.Path) == op.NewUUID || (f != nil && f[0] == "UUID="+op.NewUUID))) {
			return "", refuse("The new disk has taken %s's place and its parity is being rebuilt. Cancelling would leave the array without it: Continue instead (Stop pauses it).", op.Pool)
		}
		return fmt.Sprintf("Nothing was changed on the array; %s is still down. The new disk is unassigned.", op.Pool), nil
	case "replace-parity":
		if op.stepDone("swap") {
			return fmt.Sprintf("%s stays on the new disk; the next parity sync confirms it.", op.Pool), nil
		}
		if a.mountedUUID(ctx, op.Path) != op.OldUUID {
			return "", refuse("%s isn't on its old disk right now (the swap was interrupted). Continue to finish it.", op.Pool)
		}
		if op.TempMount != "" && a.mountedUUID(ctx, op.TempMount) != "" {
			if out, err := a.cmd(ctx, time.Minute, "umount", op.TempMount); err != nil {
				return "", fmt.Errorf("umount %s: %s", op.TempMount, strings.TrimSpace(out))
			}
		}
		if op.TempMount != "" {
			_ = os.Remove(a.sys(op.TempMount))
		}
		return fmt.Sprintf("Undone: %s stays on its old disk. The new disk keeps a partial copy and is unassigned.", op.Pool), nil
	}
	return "", nil
}

// filesOn names something on a new data disk other than its empty share
// folders ("" when there's nothing).
func filesOn(root string, folders []string) string {
	ents, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if e.Name() == ".zfs" { // ZFS's snapshot folder, when snapdir=visible
			continue
		}
		if !e.IsDir() || !slices.Contains(folders, e.Name()) {
			return e.Name()
		}
		if sub, _ := os.ReadDir(filepath.Join(root, e.Name())); len(sub) > 0 {
			return e.Name() + "/" + sub[0].Name()
		}
	}
	return ""
}

// ---------- mergerfs ----------

// mergerChange is what adding a disk does to one mergerfs mount.
type mergerChange struct {
	Target string // /mnt/user
	Live   bool   // the running mount needs the branch
	OldSrc string // the /etc/fstab line's branches
	NewSrc string // and with the new disk ("" when they already cover it)
}

// mergerPlan finds the mergerfs mounts that pool the data disks, and checks
// each can keep a new disk after a restart (it's in /etc/fstab).
func (a *Agent) mergerPlan(ctx context.Context, data []string, path string) ([]mergerChange, error) {
	o, err := a.cmd(ctx, 10*time.Second, "findmnt", "-rn", "-t", "fuse.mergerfs", "-o", "TARGET")
	if err != nil && strings.TrimSpace(o) != "" {
		return nil, fmt.Errorf("couldn't list the mergerfs mounts: %s", strings.TrimSpace(o))
	}
	fstab := a.readFstab()
	var out []mergerChange
	for target := range strings.FieldsSeq(o) {
		raw, err := a.xattr.get(filepath.Join(a.sys(target), ".mergerfs"), "user.mergerfs.branches")
		if err != nil {
			return nil, fmt.Errorf("couldn't read which disks %s uses (%v)", target, err)
		}
		branches := splitBranches(raw)
		if !slices.ContainsFunc(branches, func(b string) bool { return slices.Contains(data, b) }) {
			continue // not a view of the data disks
		}
		c := mergerChange{Target: target, Live: !slices.Contains(branches, path)}
		f := fstabEntry(fstab, target)
		if f == nil {
			return nil, fmt.Errorf("%s isn't set up in /etc/fstab, so a new disk couldn't be kept in it after a restart. Add the disk by hand", target)
		}
		c.OldSrc = f[0]
		if !covers(f[0], path) {
			c.NewSrc = f[0] + ":" + path
		}
		out = append(out, c)
	}
	return out, nil
}

// mergerRemove takes a disk back out of the mergerfs mounts (undo).
func (a *Agent) mergerRemove(ctx context.Context, op *ReplaceOp) error {
	o, _ := a.cmd(ctx, 10*time.Second, "findmnt", "-rn", "-t", "fuse.mergerfs", "-o", "TARGET")
	for target := range strings.FieldsSeq(o) {
		ctl := filepath.Join(a.sys(target), ".mergerfs")
		if raw, err := a.xattr.get(ctl, "user.mergerfs.branches"); err == nil && slices.Contains(splitBranches(raw), op.Path) {
			if err := a.xattr.set(ctl, "user.mergerfs.branches", "-"+op.Path); err != nil {
				return fmt.Errorf("couldn't take %s out of %s: %v", op.Path, target, err)
			}
		}
	}
	return a.editFstab(ctx, op, func(lines []string) ([]string, error) {
		changed := false
		for i, l := range lines {
			f := fstabFields(l)
			if len(f) < 3 || !strings.Contains(f[2], "mergerfs") {
				continue
			}
			parts := strings.Split(f[0], ":")
			if j := slices.Index(parts, op.Path); j >= 0 && len(parts) > 1 {
				lines[i], changed = replaceField(l, 0, strings.Join(slices.Delete(parts, j, j+1), ":")), true
			}
		}
		if !changed {
			return nil, nil
		}
		return lines, nil
	})
}

// splitBranches reads mergerfs's branch list ("/mnt/cache=RW:/mnt/disk1=RW").
func splitBranches(raw string) []string {
	var out []string
	for b := range strings.SplitSeq(strings.TrimSpace(raw), ":") {
		if b = strings.TrimRight(strings.SplitN(b, "=", 2)[0], "/"); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// covers says whether an fstab mergerfs source already includes path,
// directly or through a glob (/mnt/disk*).
func covers(src, path string) bool {
	for _, b := range splitBranches(src) {
		if ok, _ := filepath.Match(b, path); ok || b == path {
			return true
		}
	}
	return false
}

// xattrOps reads and sets extended attributes (mergerfs's control file).
type xattrOps struct {
	get func(path, name string) (string, error)
	set func(path, name, value string) error
}

var realXattr = xattrOps{
	get: func(path, name string) (string, error) {
		buf := make([]byte, 64<<10)
		n, err := syscall.Getxattr(path, name, buf)
		if err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	},
	set: func(path, name, value string) error { return syscall.Setxattr(path, name, []byte(value), 0) },
}

// ---------- Changing /etc/fstab and snapraid.conf ----------

func (a *Agent) fstabPath() string { return a.sys("/etc/fstab") }

func (a *Agent) readFstab() string { return readText(a.fstabPath()) }

func readText(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// fstabFields splits an fstab line (nil for comments and blank lines).
func fstabFields(line string) []string {
	f := strings.Fields(line)
	if len(f) < 3 || strings.HasPrefix(f[0], "#") {
		return nil
	}
	return f
}

// fstabEntry finds the line that mounts target.
func fstabEntry(text, target string) []string {
	for l := range strings.SplitSeq(text, "\n") {
		if f := fstabFields(l); f != nil && f[1] == target {
			return f
		}
	}
	return nil
}

// replaceField swaps one whitespace-separated field, keeping the spacing.
func replaceField(line string, idx int, val string) string {
	n := 0
	for i := 0; i < len(line); {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		j := i
		for j < len(line) && line[j] != ' ' && line[j] != '\t' {
			j++
		}
		if i == j {
			break
		}
		if n == idx {
			return line[:i] + val + line[j:]
		}
		n, i = n+1, j
	}
	return line
}

// fileGuard says where a file is backed up before it's first changed
// (path.homestack-<Suffix>), and who hears about the copy.
type fileGuard struct {
	Suffix string
	Saved  func(backup string)
}

// opGuard backs files up for a disk job, listed in the job.
func (a *Agent) opGuard(op *ReplaceOp) *fileGuard {
	return &fileGuard{Suffix: op.ID, Saved: func(b string) {
		op.Backups = append(op.Backups, b)
		a.saveOp(op)
	}}
}

// editFstab changes /etc/fstab for a disk job (see editFstabG).
func (a *Agent) editFstab(ctx context.Context, op *ReplaceOp, change func(lines []string) ([]string, error)) error {
	return a.editFstabG(ctx, a.opGuard(op), change)
}

// editFstabG changes /etc/fstab through change (nil lines: no change). The
// result has to pass findmnt --verify with no errors the old file didn't
// have, or it isn't used.
func (a *Agent) editFstabG(ctx context.Context, g *fileGuard, change func(lines []string) ([]string, error)) error {
	path := a.fstabPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("couldn't read /etc/fstab: %v", err)
	}
	next, err := change(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))
	if err != nil || next == nil {
		return err
	}
	text := strings.Join(next, "\n") + "\n"
	if text == string(b) {
		return nil
	}
	err = a.replaceChecked(g, path, text, func(tmp string) error {
		before, err := a.fstabErrors(ctx, path)
		if err != nil {
			return err
		}
		after, err := a.fstabErrors(ctx, tmp)
		if err != nil {
			return err
		}
		for _, e := range after {
			if !slices.Contains(before, e) {
				return fmt.Errorf("the new /etc/fstab didn't pass findmnt --verify (%s), so it wasn't changed", e)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, _ = a.cmd(ctx, time.Minute, "systemctl", "daemon-reload")
	return nil
}

// fstabErrors are findmnt --verify's errors for an fstab file.
func (a *Agent) fstabErrors(ctx context.Context, file string) ([]string, error) {
	out, err := a.cmd(ctx, 30*time.Second, "findmnt", "--verify", "--tab-file", file)
	var errs []string
	target := ""
	for l := range strings.SplitSeq(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
		case l[0] != ' ' && l[0] != '\t':
			target = t
		case strings.HasPrefix(t, "[E]"):
			errs = append(errs, target+": "+strings.TrimSpace(t[3:]))
		}
	}
	if err != nil && len(errs) == 0 && !strings.Contains(out, "error") {
		// A check that can't run counts as failing.
		return nil, fmt.Errorf("couldn't check /etc/fstab (findmnt --verify: %s)", strings.TrimSpace(out))
	}
	return errs, nil
}

// replaceChecked writes text to path through a temporary file that check
// approves, after backing the old file up (once per guard, next to it).
func (a *Agent) replaceChecked(g *fileGuard, path, text string, check func(tmp string) error) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := a.backupOnce(g, path); err != nil {
		return err
	}
	tmp := path + ".homestack-tmp"
	if err := os.WriteFile(tmp, []byte(text), fi.Mode().Perm()); err != nil {
		return err
	}
	if err := check(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		f.Close()
	}
	return os.Rename(tmp, path)
}

// backupOnce copies path to path.homestack-<suffix> before it's first
// changed, so the file as it was before the job is always there.
func (a *Agent) backupOnce(g *fileGuard, path string) error {
	bak := path + ".homestack-" + g.Suffix
	if _, err := os.Stat(bak); err == nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fi, _ := os.Stat(path)
	if err := os.WriteFile(bak, b, fi.Mode().Perm()); err != nil {
		return err
	}
	if g.Saved != nil {
		g.Saved(strings.TrimPrefix(bak, a.opt.SysRoot))
	}
	return nil
}

// addSnapLine puts line into snapraid.conf after the last line of its kind
// (data or parity), checked by reading the result back.
func (a *Agent) addSnapLine(op *ReplaceOp, kind, line string) error {
	path := a.snapraidConf()
	before, err := readSnapConf(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(readText(path), "\n"), "\n")
	last, firstData := -1, -1
	for i, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		isParity := f[0] == "parity" || strings.HasSuffix(f[0], "-parity")
		isData := f[0] == "data" || f[0] == "disk"
		if (kind == "parity" && isParity) || (kind == "data" && isData) {
			last = i
		}
		if firstData < 0 && (isData || f[0] == "content") {
			firstData = i
		}
	}
	at := len(lines)
	switch {
	case last >= 0:
		at = last + 1
	case kind == "parity" && firstData >= 0:
		at = firstData
	}
	text := strings.Join(slices.Insert(lines, at, line), "\n") + "\n"
	return a.replaceChecked(a.opGuard(op), path, text, func(tmp string) error {
		c, err := readSnapConf(tmp)
		if err != nil {
			return err
		}
		d, p := len(before.data), len(before.parity)
		if kind == "data" {
			d++
		} else {
			p++
		}
		if len(c.data) != d || len(c.parity) != p || len(c.content) != len(before.content) {
			return errors.New("snapraid.conf didn't read back as expected, so it wasn't changed")
		}
		return nil
	})
}

// removeSnapLine takes the lines match picks out of snapraid.conf (undo).
func (a *Agent) removeSnapLine(op *ReplaceOp, match func(f []string) bool) error {
	path := a.snapraidConf()
	text := readText(path)
	var keep []string
	removed := 0
	for l := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if f := strings.Fields(l); len(f) > 0 && !strings.HasPrefix(f[0], "#") && match(f) {
			removed++
			continue
		}
		keep = append(keep, l)
	}
	if removed == 0 {
		return nil
	}
	before, err := readSnapConf(path)
	if err != nil {
		return err
	}
	return a.replaceChecked(a.opGuard(op), path, strings.Join(keep, "\n")+"\n", func(tmp string) error {
		c, err := readSnapConf(tmp)
		if err != nil {
			return err
		}
		if len(c.data)+len(c.parity) != len(before.data)+len(before.parity)-removed {
			return errors.New("snapraid.conf didn't read back as expected, so it wasn't changed")
		}
		return nil
	})
}

// ---------- API ----------

func (a *Agent) getArray(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.planAssign(r.Context()).view)
}

func (a *Agent) postArray(w http.ResponseWriter, r *http.Request) {
	var req assignReq
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if err := a.assignStart(r.Context(), req); err != nil {
		if rf, ok := errors.AsType[refusal](err); ok {
			writeErr(w, 409, rf.msg)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	a.refresh(context.Background())
	writeJSON(w, 200, a.planAssign(r.Context()).view)
}
