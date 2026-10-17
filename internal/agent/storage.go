package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pool is one ZFS pool.
type Pool struct {
	Name        string   `json:"name"`
	Size        uint64   `json:"size"`
	Alloc       uint64   `json:"alloc"`
	Free        uint64   `json:"free"`
	Frag        int      `json:"frag"`
	Cap         int      `json:"cap"`
	Health      string   `json:"health"`
	Array       bool     `json:"array"`
	Mountpoint  string   `json:"mountpoint"`
	Mounted     bool     `json:"mounted"`
	Scan        string   `json:"scan"`      // the "scan:" line, tidied
	ScanState   string   `json:"scanState"` // scrubbing, resilvering, paused, idle
	ScanPercent *float64 `json:"scanPercent,omitempty"`
	ScanETA     string   `json:"scanEta,omitempty"`
	Errors      string   `json:"errors"`
	Devices     []string `json:"devices"`
	Datasets    int      `json:"datasets"` // child datasets (shares not yet flattened)
}

// Filesystem is a mounted non-ZFS filesystem (the cache, mergerfs).
type Filesystem struct {
	Mount  string `json:"mount"`
	Source string `json:"source"`
	FSType string `json:"fstype"`
	Size   uint64 `json:"size"`
	Used   uint64 `json:"used"`
	Avail  uint64 `json:"avail"`
}

// ArrayStatus says whether the array is up.
type ArrayStatus struct {
	State   string          `json:"state"` // started, stopped, partial
	Mounts  map[string]bool `json:"mounts"`
	Busy    string          `json:"busy,omitempty"` // what holds the array lock
	Stopped *time.Time      `json:"stoppedAt,omitempty"`
}

func (a *Agent) pools(ctx context.Context) []Pool {
	out, err := a.cmd(ctx, 10*time.Second, "zpool", "list", "-Hp", "-o", "name,size,alloc,free,frag,cap,health")
	if err != nil {
		return []Pool{}
	}
	pools := []Pool{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		p := Pool{Name: f[0], Health: f[6], Array: true}
		p.Size, _ = strconv.ParseUint(f[1], 10, 64)
		p.Alloc, _ = strconv.ParseUint(f[2], 10, 64)
		p.Free, _ = strconv.ParseUint(f[3], 10, 64)
		p.Frag, _ = strconv.Atoi(strings.TrimSuffix(f[4], "%"))
		p.Cap, _ = strconv.Atoi(strings.TrimSuffix(f[5], "%"))
		a.poolDetail(ctx, &p)
		pools = append(pools, p)
	}
	return pools
}

var (
	reScanPct = regexp.MustCompile(`([\d.]+)% done`)
	reScanETA = regexp.MustCompile(`, ([\d:]+|[\d]+ days [\d:]+) to go`)
)

func (a *Agent) poolDetail(ctx context.Context, p *Pool) {
	if o, err := a.cmd(ctx, 5*time.Second, "zfs", "get", "-H", "-o", "value", "mountpoint,mounted", p.Name); err == nil {
		v := strings.Fields(o)
		if len(v) == 2 {
			p.Mountpoint, p.Mounted = v[0], v[1] == "yes"
		}
	}
	if o, err := a.cmd(ctx, 5*time.Second, "zfs", "list", "-H", "-d", "1", "-o", "name", p.Name); err == nil {
		p.Datasets = len(strings.Fields(o)) - 1
	}
	o, err := a.cmd(ctx, 10*time.Second, "zpool", "status", "-P", p.Name)
	if err != nil {
		return
	}
	p.ScanState = "idle"
	var scan []string
	inScan, inConfig := false, false
	sc := bufio.NewScanner(strings.NewReader(o))
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "scan:"):
			inScan = true
			scan = append(scan, strings.TrimSpace(strings.TrimPrefix(t, "scan:")))
			continue
		case strings.HasPrefix(t, "config:"):
			inScan, inConfig = false, true
			continue
		case strings.HasPrefix(t, "errors:"):
			inConfig = false
			p.Errors = strings.TrimSpace(strings.TrimPrefix(t, "errors:"))
			continue
		case strings.HasSuffix(t, ":") && !strings.HasPrefix(t, "/"):
			inScan = false
		}
		if inScan && t != "" {
			scan = append(scan, t)
		}
		if inConfig {
			f := strings.Fields(t)
			if len(f) > 0 && strings.HasPrefix(f[0], "/dev/") {
				p.Devices = append(p.Devices, f[0])
			}
		}
	}
	p.Scan = strings.Join(scan, " ")
	low := strings.ToLower(p.Scan)
	switch {
	case strings.Contains(low, "resilver in progress"):
		p.ScanState = "resilvering"
	case strings.Contains(low, "scrub paused"):
		p.ScanState = "paused"
	case strings.Contains(low, "scrub in progress"):
		p.ScanState = "scrubbing"
	}
	if m := reScanPct.FindStringSubmatch(p.Scan); m != nil && p.ScanState != "idle" {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			p.ScanPercent = &v
		}
	}
	if m := reScanETA.FindStringSubmatch(p.Scan); m != nil {
		p.ScanETA = m[1]
	}
}

func (a *Agent) fsInfo(ctx context.Context, mount string) *Filesystem {
	o, err := a.cmd(ctx, 5*time.Second, "findmnt", "-J", "-b", "-o", "TARGET,SOURCE,FSTYPE,SIZE,USED,AVAIL", "--mountpoint", mount)
	if err != nil {
		return nil
	}
	var r struct {
		Filesystems []struct {
			Target string `json:"target"`
			Source string `json:"source"`
			FSType string `json:"fstype"`
			Size   any    `json:"size"`
			Used   any    `json:"used"`
			Avail  any    `json:"avail"`
		} `json:"filesystems"`
	}
	if json.Unmarshal([]byte(o), &r) != nil || len(r.Filesystems) == 0 {
		return nil
	}
	f := r.Filesystems[0]
	return &Filesystem{Mount: f.Target, Source: f.Source, FSType: f.FSType, Size: asUint(f.Size), Used: asUint(f.Used), Avail: asUint(f.Avail)}
}

func (a *Agent) cacheFS(ctx context.Context) *Filesystem { return a.fsInfo(ctx, a.layout().Cache) }

func (a *Agent) isMounted(ctx context.Context, path string) bool {
	_, err := a.cmd(ctx, 5*time.Second, "findmnt", "--mountpoint", path)
	return err == nil
}

func (a *Agent) arrayStatus(ctx context.Context, pools []Pool) ArrayStatus {
	st := ArrayStatus{Mounts: map[string]bool{}}
	up, down := 0, 0
	for _, p := range pools {
		if p.Array {
			if p.Mounted {
				up++
			} else {
				down++
			}
		}
	}
	for _, m := range append(a.layout().arrayMounts(), a.plainDataDisks(ctx)...) {
		ok := a.isMounted(ctx, m)
		st.Mounts[m] = ok
		if ok {
			up++
		} else {
			down++
		}
	}
	switch {
	case down == 0:
		st.State = "started"
	case up == 0:
		st.State = "stopped"
	default:
		st.State = "partial"
	}
	st.Busy = a.jobs.lockHolder()
	if t := a.readTime("array-stopped"); t != nil && st.State != "started" {
		st.Stopped = t
	}
	return st
}

// ---------- SnapRAID ----------

// ParityStatus is SnapRAID's state.
type ParityStatus struct {
	Installed     bool         `json:"installed"`
	Configured    bool         `json:"configured"`
	Synced        bool         `json:"synced"`  // at least one sync has completed
	Syncing       bool         `json:"syncing"` // a sync is running right now
	Version       string       `json:"version,omitempty"`
	Parity        []ParityDisk `json:"parity"`
	Data          []ParityDisk `json:"data"`
	LastSync      *time.Time   `json:"lastSync,omitempty"`
	DaysSinceSync int          `json:"daysSinceSync"`
	LastCheck     *JobResult   `json:"lastCheck,omitempty"`
	LastRun       *JobResult   `json:"lastRun,omitempty"` // the last sync's outcome (a skipped one says why)
	Summary       []string     `json:"summary"`
	Errors        string       `json:"errors,omitempty"`
	Checked       *time.Time   `json:"checked,omitempty"`
}

// ParityDisk is one data or parity entry from snapraid.conf.
type ParityDisk struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Mounted bool   `json:"mounted"`
}

type snapConf struct {
	parity, data []ParityDisk
	content      []string
}

func (a *Agent) snapraidConf() string {
	if v := os.Getenv("HOMESTACK_SNAPRAID_CONF"); v != "" {
		return v
	}
	return a.layout().SnapRAID
}

func readSnapConf(path string) (*snapConf, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &snapConf{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.Fields(t)
		switch {
		case (f[0] == "parity" || strings.HasSuffix(f[0], "-parity")) && len(f) >= 2:
			for i, p := range strings.Split(f[1], ",") {
				c.parity = append(c.parity, ParityDisk{Name: f[0] + suffix(i), Path: p})
			}
		case (f[0] == "data" || f[0] == "disk") && len(f) >= 3:
			c.data = append(c.data, ParityDisk{Name: f[1], Path: f[2]})
		case f[0] == "content" && len(f) >= 2:
			c.content = append(c.content, f[1])
		}
	}
	return c, sc.Err()
}

func suffix(i int) string {
	if i == 0 {
		return ""
	}
	return "#" + strconv.Itoa(i+1)
}

var parityCache struct {
	sync.Mutex
	at      time.Time
	summary []string
	errors  string
}

func (a *Agent) parityStatus(ctx context.Context) ParityStatus {
	ps := ParityStatus{Parity: []ParityDisk{}, Data: []ParityDisk{}, Summary: []string{}}
	if o, err := a.cmd(ctx, 5*time.Second, "snapraid", "--version"); err == nil {
		ps.Installed = true
		ps.Version = strings.TrimSpace(strings.SplitN(o, "\n", 2)[0])
	}
	conf, err := readSnapConf(a.snapraidConf())
	if err != nil {
		return ps
	}
	ps.Configured = true
	for _, d := range conf.data {
		d.Mounted = a.isMounted(ctx, strings.TrimRight(d.Path, "/"))
		ps.Data = append(ps.Data, d)
	}
	for _, d := range conf.parity {
		d.Mounted = a.isMounted(ctx, parentMount(d.Path))
		ps.Parity = append(ps.Parity, d)
	}
	// When was parity last complete? The last successful sync recorded by the
	// agent or snapraid-job. The content file's time is only a fallback for
	// syncs run by hand: SnapRAID also saves it partway through a sync
	// (autosave), so on its own it would say "up to date" too early.
	ps.Syncing = a.jobs.runningAction() == "parity-sync" || strings.HasPrefix(a.jobs.lockHolder(), "Parity sync")
	if r := a.jobs.lastOK("parity-sync"); r != nil {
		t := r.Finished
		ps.LastSync = &t
	} else if a.jobs.lastResult("parity-sync") == nil && !ps.Syncing {
		if newest := newestFile(conf.content); !newest.IsZero() {
			ps.LastSync = &newest
		}
	}
	if ps.LastSync != nil {
		ps.Synced = true
		ps.DaysSinceSync = int(time.Since(*ps.LastSync).Hours() / 24)
	}
	ps.LastCheck = a.jobs.lastResult("parity-check")
	ps.LastRun = a.jobs.lastResult("parity-sync")

	// `snapraid status` reads the whole content file, so it's read every 30
	// minutes, and again once a sync or check has finished since, or SnapRAID
	// has saved the content file (a run by hand); never while a job holds the
	// array or SnapRAID is running.
	changed := []time.Time{newestFile(conf.content)}
	for _, r := range []*JobResult{ps.LastRun, ps.LastCheck} {
		if r != nil {
			changed = append(changed, r.Finished)
		}
	}
	parityCache.Lock()
	defer parityCache.Unlock()
	if ps.Installed && ps.Synced && statusStale(parityCache.at, time.Now(), changed...) && a.jobs.lockHolder() == "" && !a.snapraidRunning(ctx) {
		parityCache.at = time.Now()
		o, _ := a.cmd(ctx, 5*time.Minute, "snapraid", "-c", a.snapraidConf(), "status")
		if s, e := parseSnapStatus(o); len(s) > 0 { // else keep the last one
			parityCache.summary, parityCache.errors = s, e
		}
	}
	ps.Summary = append(ps.Summary, parityCache.summary...)
	ps.Errors = parityCache.errors
	if !parityCache.at.IsZero() {
		t := parityCache.at
		ps.Checked = &t
	}
	return ps
}

// statusStale says whether `snapraid status`, last read at, should be read
// again: after 30 minutes, or once anything in changed is newer.
func statusStale(at, now time.Time, changed ...time.Time) bool {
	if now.Sub(at) > 30*time.Minute {
		return true
	}
	for _, t := range changed {
		if t.After(at) {
			return true
		}
	}
	return false
}

// newestFile is the latest modification time among the files that exist.
func newestFile(paths []string) time.Time {
	var newest time.Time
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// snapraidRunning says whether SnapRAID is running (started anywhere,
// a terminal included).
func (a *Agent) snapraidRunning(ctx context.Context) bool {
	out, err := a.cmd(ctx, 5*time.Second, "pgrep", "-x", "snapraid")
	return err == nil && strings.TrimSpace(out) != ""
}

func parentMount(p string) string {
	// /mnt/parity1/snapraid.parity -> /mnt/parity1
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

func parseSnapStatus(out string) (summary []string, errs string) {
	for line := range strings.SplitSeq(out, "\n") {
		t := strings.TrimSpace(line)
		low := strings.ToLower(t)
		switch {
		case strings.HasPrefix(t, "DANGER"), strings.Contains(low, "unrecoverable"):
			errs = t
			summary = append(summary, t)
		case strings.Contains(low, "scrubbed"), strings.Contains(low, "sync is in progress"),
			strings.Contains(low, "no error detected"), strings.Contains(low, "error"),
			strings.Contains(low, "the array is"), strings.Contains(low, "rehash"),
			strings.Contains(low, "is not scrubbed"):
			if len(t) < 200 && !strings.HasPrefix(t, "Self test") && !strings.HasPrefix(t, "Files") {
				summary = append(summary, t)
			}
		}
	}
	return summary, errs
}
