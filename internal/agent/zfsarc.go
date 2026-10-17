package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The ZFS cache (ARC) limit. ZFS keeps recently used data in RAM and gives
// it back when programs need memory, but slowly; since OpenZFS 2.3 it may
// grow to all but 1 GB of RAM by default, which squeezes VMs and
// containers. The limit is set now (/sys/module/zfs/parameters) and for
// every boot (/etc/modprobe.d), and the boot image is refreshed when ZFS is
// part of it.

const (
	arcStatsPath  = "/proc/spl/kstat/zfs/arcstats"
	arcParamDir   = "/sys/module/zfs/parameters"
	modprobeDir   = "/etc/modprobe.d"
	modprobeFile  = "/etc/modprobe.d/zfs.conf"
	initramfsHook = "/usr/share/initramfs-tools/hooks/zfs"
	minARC        = 1 << 30 // below this ZFS itself struggles
	arcComment    = "# ZFS cache (ARC) limit, set in Homestack under Settings > Storage"
	oldARCComment = "# ZFS cache (ARC) limit, set from Homestack's Storage page" // up to v0.15.2
)

// ARCView is what Settings > Storage shows.
type ARCView struct {
	Available bool    `json:"available"`
	Size      uint64  `json:"size"`     // in use now
	Target    uint64  `json:"target"`   // where ZFS is heading (c)
	Min       uint64  `json:"min"`      // c_min
	Max       uint64  `json:"max"`      // c_max: the limit in force
	Limit     uint64  `json:"limit"`    // zfs_arc_max as set: 0 = ZFS's default
	Saved     uint64  `json:"saved"`    // in /etc/modprobe.d (for boot): 0 = none
	SavedIn   string  `json:"savedIn"`  // which file
	MemTotal  uint64  `json:"memTotal"` // RAM
	HitRate   float64 `json:"hitRate"`  // since boot, percent
	MinLimit  uint64  `json:"minLimit"` // smallest limit accepted here
	MaxLimit  uint64  `json:"maxLimit"` // largest
	Boot      string  `json:"boot"`     // boot image: "" (not needed), updating, ok, failed: ...
}

type arcManager struct {
	a *Agent
	// writeSys writes a kernel parameter (a fake in tests).
	writeSys func(path string, data []byte) error

	setMu sync.Mutex // one change at a time

	mu       sync.Mutex
	boot     string
	updating bool
	again    bool
}

func newARCManager(a *Agent) *arcManager {
	return &arcManager{a: a, writeSys: func(p string, b []byte) error { return os.WriteFile(p, b, 0) }}
}

func (m *arcManager) view() ARCView {
	a := m.a
	st := readKstat(a.sys(arcStatsPath))
	v := ARCView{Available: st["c_max"] > 0}
	if !v.Available {
		return v
	}
	v.Size, v.Target, v.Min, v.Max = st["size"], st["c"], st["c_min"], st["c_max"]
	if h, mi := st["hits"], st["misses"]; h+mi > 0 {
		v.HitRate = float64(h) / float64(h+mi) * 100
	}
	v.MemTotal = st["memory_all_bytes"]
	if v.MemTotal == 0 {
		v.MemTotal = memTotal(a.sys("/proc/meminfo"))
	}
	v.Limit = readParam(a.sys(filepath.Join(arcParamDir, "zfs_arc_max")))
	v.Saved, v.SavedIn = savedARCMax(a.sys(modprobeDir))
	if a.opt.SysRoot != "" { // the path on the server, not where tests keep it
		v.SavedIn = strings.TrimPrefix(v.SavedIn, filepath.Clean(a.opt.SysRoot))
	}
	v.MinLimit = minARC
	if v.MemTotal > 2*minARC {
		v.MaxLimit = v.MemTotal - minARC
	}
	m.mu.Lock()
	v.Boot = m.boot
	m.mu.Unlock()
	return v
}

func readKstat(path string) map[string]uint64 {
	out := map[string]uint64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 3 {
			if n, err := strconv.ParseUint(f[2], 10, 64); err == nil {
				out[f[0]] = n
			}
		}
	}
	return out
}

func readParam(path string) uint64 {
	b, _ := os.ReadFile(path)
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func memTotal(path string) uint64 {
	b, _ := os.ReadFile(path)
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			kb, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb * 1024
		}
	}
	return 0
}

// savedARCMax finds zfs_arc_max in the modprobe settings (the last one wins,
// as when the module loads).
func savedARCMax(dir string) (uint64, string) {
	var val uint64
	var in string
	files, _ := filepath.Glob(filepath.Join(dir, "*.conf"))
	slices.Sort(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			fl := strings.Fields(line)
			if len(fl) < 3 || fl[0] != "options" || fl[1] != "zfs" {
				continue
			}
			for _, tok := range fl[2:] {
				if v, ok := strings.CutPrefix(tok, "zfs_arc_max="); ok {
					val, _ = strconv.ParseUint(v, 10, 64)
					in = f
				}
			}
		}
	}
	return val, in
}

// setOption puts name=value into a modprobe file's `options zfs` lines,
// keeping everything else, or takes it out (value 0). It reports whether
// the name was there.
func setOption(content, name string, value uint64) (string, bool) {
	lines := strings.Split(content, "\n")
	found, placed := false, false
	var out []string
	for _, line := range lines {
		fl := strings.Fields(line)
		if len(fl) < 2 || fl[0] != "options" || fl[1] != "zfs" {
			out = append(out, line)
			continue
		}
		var keep []string
		for _, tok := range fl[2:] {
			if strings.HasPrefix(tok, name+"=") {
				found = true
				if value > 0 && !placed {
					keep = append(keep, fmt.Sprintf("%s=%d", name, value))
					placed = true
				}
				continue
			}
			keep = append(keep, tok)
		}
		if len(keep) > 0 {
			out = append(out, "options zfs "+strings.Join(keep, " "))
		}
	}
	return strings.Join(out, "\n"), found
}

// save writes the limit (0 = none) for every boot: in place wherever it's
// set already, otherwise in zfs.conf.
func (m *arcManager) save(max, min uint64) error {
	files, _ := filepath.Glob(filepath.Join(m.a.sys(modprobeDir), "*.conf"))
	slices.Sort(files)
	write := func(f, text string) error {
		text = strings.ReplaceAll(text, oldARCComment+"\n", arcComment+"\n")
		if max == 0 { // our note goes with our setting
			text = strings.ReplaceAll(text, arcComment+"\n", "")
		}
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if strings.TrimSpace(text) == "" {
			return os.Remove(f) // nothing left
		}
		tmp := f + ".homestack-tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, f)
	}
	params := []struct {
		name string
		val  uint64
	}{{"zfs_arc_max", max}}
	if min > 0 {
		params = append(params, struct {
			name string
			val  uint64
		}{"zfs_arc_min", min})
	}
	// First wherever they're set already: the first file keeps the value,
	// later ones (which would win at boot) lose theirs.
	placed := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		text, changed := string(b), false
		for _, p := range params {
			v := p.val
			if placed[p.name] {
				v = 0
			}
			if next, found := setOption(text, p.name, v); found {
				text, changed = next, true
				placed[p.name] = placed[p.name] || v > 0
			}
		}
		if changed {
			if err := write(f, text); err != nil {
				return err
			}
		}
	}
	// Otherwise in zfs.conf.
	var add []string
	for _, p := range params {
		if p.val > 0 && !placed[p.name] {
			add = append(add, fmt.Sprintf("%s=%d", p.name, p.val))
		}
	}
	if len(add) == 0 {
		return nil
	}
	main := m.a.sys(modprobeFile)
	b, err := os.ReadFile(main)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := strings.TrimRight(string(b), "\n")
	if text != "" {
		text += "\n"
	}
	return write(main, text+arcComment+"\noptions zfs "+strings.Join(add, " ")+"\n")
}

// set applies a new limit now and for every boot. 0 goes back to ZFS's own
// default.
func (m *arcManager) set(max uint64) (ARCView, error) {
	m.setMu.Lock()
	defer m.setMu.Unlock()
	v := m.view()
	if !v.Available {
		return v, refuse("ZFS isn't loaded on this server.")
	}
	if max != 0 && (max < v.MinLimit || (v.MaxLimit > 0 && max > v.MaxLimit)) {
		return v, refuse("Pick a limit between %s and %s.", gib(v.MinLimit), gib(v.MaxLimit))
	}
	param := func(name string) string { return m.a.sys(filepath.Join(arcParamDir, name)) }
	// ZFS ignores a limit at or below its minimum; lower the minimum with it.
	var newMin uint64
	if max != 0 && max <= v.Min {
		newMin = max / 2
		if err := m.writeSys(param("zfs_arc_min"), []byte(strconv.FormatUint(newMin, 10))); err != nil {
			return v, fmt.Errorf("couldn't lower the cache minimum: %v", err)
		}
	}
	oldLimit := v.Limit
	if err := m.writeSys(param("zfs_arc_max"), []byte(strconv.FormatUint(max, 10))); err != nil {
		return v, fmt.Errorf("couldn't set the limit: %v", err)
	}
	// ZFS quietly ignores values it doesn't like; check it took this one.
	if max != 0 {
		ok := false
		for range 10 {
			if readKstat(m.a.sys(arcStatsPath))["c_max"] == max {
				ok = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ok {
			_ = m.writeSys(param("zfs_arc_max"), []byte(strconv.FormatUint(oldLimit, 10)))
			return m.view(), refuse("ZFS didn't accept %s as the limit, so nothing was changed.", gib(max))
		}
	}
	if err := m.save(max, newMin); err != nil {
		return m.view(), fmt.Errorf("the limit is in force now, but couldn't be saved for the next boot: %v", err)
	}
	m.refreshBoot()
	return m.view(), nil
}

// refreshBoot rebuilds the boot image when ZFS is in it, so the limit is
// there from the first moment ZFS loads. It runs in the background.
func (m *arcManager) refreshBoot() {
	if _, err := os.Stat(m.a.sys(initramfsHook)); err != nil {
		return
	}
	m.mu.Lock()
	if m.updating {
		m.again = true
		m.mu.Unlock()
		return
	}
	m.updating, m.boot = true, "updating"
	m.mu.Unlock()
	go func() {
		for {
			out, err := m.a.cmd(context.Background(), 10*time.Minute, "update-initramfs", "-u", "-k", "all")
			m.mu.Lock()
			if err != nil {
				msg := lastLine(out)
				if msg == "" {
					msg = err.Error()
				}
				m.boot = "failed: " + msg
			} else {
				m.boot = "ok"
			}
			if !m.again {
				m.updating = false
				m.mu.Unlock()
				return
			}
			m.again = false
			m.boot = "updating"
			m.mu.Unlock()
		}
	}()
}

func gib(b uint64) string {
	if b%(1<<30) == 0 {
		return fmt.Sprintf("%d GB", b>>30)
	}
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}

func (m *arcManager) getARC(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, m.view())
}

func (m *arcManager) putARC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Max uint64 `json:"max"` // bytes; 0 = ZFS's default
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	v, err := m.set(req.Max)
	if err != nil {
		if rf, ok := errors.AsType[refusal](err); ok {
			writeErr(w, 409, rf.msg)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}
