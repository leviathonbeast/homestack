package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-disk reads and writes, like Unraid's Main tab, from the kernel's own
// counters (/proc/diskstats). Reading them never touches a disk, so they
// can be sampled every few seconds, and they're what spin-down (spindown.go)
// watches for a disk being idle.

type ioCounters struct {
	reads, writes           uint64 // completed requests
	readBytes, writtenBytes uint64
}

type ioTracker struct {
	mu     sync.Mutex
	last   map[string]ioCounters
	at     time.Time
	rates  map[string][2]float64 // bytes/s read, written
	active map[string]time.Time  // last time a disk did anything
}

func newIOTracker() *ioTracker {
	return &ioTracker{last: map[string]ioCounters{}, rates: map[string][2]float64{}, active: map[string]time.Time{}}
}

// readDiskstats parses /proc/diskstats: name, then reads completed (4th
// field), sectors read (6th), writes completed (8th), sectors written
// (10th). Sectors are always 512 bytes there.
func readDiskstats(path string) map[string]ioCounters {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]ioCounters{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		out[f[2]] = ioCounters{reads: n(3), readBytes: n(5) * 512, writes: n(7), writtenBytes: n(9) * 512}
	}
	return out
}

// sample takes a new reading and works out each disk's rate since the last.
func (t *ioTracker) sample(path string, now time.Time) {
	cur := readDiskstats(path)
	if cur == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	secs := now.Sub(t.at).Seconds()
	for name, c := range cur {
		prev, seen := t.last[name]
		switch {
		case !seen:
			t.active[name] = now // nothing to compare with yet: count it as busy
		case c.reads != prev.reads || c.writes != prev.writes:
			t.active[name] = now
		}
		if seen && secs > 0 && c.readBytes >= prev.readBytes && c.writtenBytes >= prev.writtenBytes {
			t.rates[name] = [2]float64{float64(c.readBytes-prev.readBytes) / secs, float64(c.writtenBytes-prev.writtenBytes) / secs}
		}
	}
	t.last, t.at = cur, now
}

// fill puts the latest figures on the disks.
func (t *ioTracker) fill(disks []Disk, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range disks {
		d := &disks[i]
		r := t.rates[d.Name]
		c := t.last[d.Name]
		d.ReadBps, d.WriteBps, d.ReadBytes, d.WrittenBytes = r[0], r[1], c.readBytes, c.writtenBytes
		if at, ok := t.active[d.Name]; ok {
			idle := int64(now.Sub(at).Seconds())
			d.IdleSec = &idle
			if r[0] > 0 || r[1] > 0 {
				d.Asleep = false // it's been woken since SMART last looked
			}
		}
	}
}

// idleFor is how long a disk has done nothing (0 if it's never been seen).
func (t *ioTracker) idleFor(name string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.active[name]
	if !ok {
		return 0
	}
	return now.Sub(at)
}
