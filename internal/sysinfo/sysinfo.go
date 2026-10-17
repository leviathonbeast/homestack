// Package sysinfo reads host CPU, memory, temperature, network, disk I/O and
// disk space figures from /proc, /sys and statfs. Inside a container /proc
// and /sys still report the host's numbers (Homestack uses host networking,
// so /sys/class/net and /proc/net/dev are the host's too).
package sysinfo

import (
	"bufio"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Host is a snapshot for the dashboard.
type Host struct {
	CPUPercent float64    `json:"cpuPercent"`
	CPUModel   string     `json:"cpuModel,omitempty"`
	CPUThreads int        `json:"cpuThreads"`
	CPUTemp    *float64   `json:"cpuTemp,omitempty"` // °C
	Load1      float64    `json:"load1"`
	Load5      float64    `json:"load5"`
	Load15     float64    `json:"load15"`
	Procs      int        `json:"procs"`
	MemTotal   uint64     `json:"memTotal"`
	MemUsed    uint64     `json:"memUsed"`  // in use by programs: not cache, not the ZFS ARC
	MemCache   uint64     `json:"memCache"` // page cache the kernel gives back when needed
	MemARC     uint64     `json:"memArc"`   // ZFS ARC, also given back when needed
	MemARCMax  uint64     `json:"memArcMax,omitempty"`
	MemFree    uint64     `json:"memFree"`
	SwapTotal  uint64     `json:"swapTotal"`
	SwapUsed   uint64     `json:"swapUsed"`
	UptimeSec  float64    `json:"uptimeSec"`
	NetRx      float64    `json:"netRx"` // bytes/s across the physical network ports
	NetTx      float64    `json:"netTx"`
	DiskRead   float64    `json:"diskRead"` // bytes/s across the physical disks
	DiskWrite  float64    `json:"diskWrite"`
	Ifaces     []Iface    `json:"ifaces"`
	Disks      []DiskInfo `json:"disks"`
}

// Iface is one physical network port.
type Iface struct {
	Name  string  `json:"name"`
	Up    bool    `json:"up"`
	Speed int     `json:"speed,omitempty"` // Mb/s
	Rx    float64 `json:"rx"`              // bytes/s
	Tx    float64 `json:"tx"`
}

// DiskInfo is the usage of the filesystem holding a path.
type DiskInfo struct {
	Label string `json:"label"`
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

// Point is one sample in the dashboard's history.
type Point struct {
	T         int64    `json:"t"` // unix seconds
	CPU       float64  `json:"cpu"`
	Mem       float64  `json:"mem"` // percent in use by programs
	NetRx     float64  `json:"netRx"`
	NetTx     float64  `json:"netTx"`
	DiskRead  float64  `json:"diskRead"`
	DiskWrite float64  `json:"diskWrite"`
	CPUTemp   *float64 `json:"cpuTemp,omitempty"`
}

// HistoryLen is 15 minutes of samples taken every 5 seconds.
const HistoryLen = 180

// Sampler works out rates (CPU, network, disk I/O) between one sample and
// the next, and keeps a short history for the dashboard's sparklines.
type Sampler struct {
	root string // "" = the real /proc and /sys; tests use a fake tree

	mu        sync.Mutex
	prevAt    time.Time
	prevCPU   [2]uint64             // all, idle
	prevNet   map[string][2]uint64  // rx, tx bytes
	rates     map[string][2]float64 // rx, tx bytes/s for every interface
	prevDisk  [2]uint64             // sectors read, written
	cpuModel  string
	cpuThread int
	last      Host
	hist      []Point
}

// NewSampler reads from root ("" for the real system).
func NewSampler(root string) *Sampler {
	s := &Sampler{root: root, prevNet: map[string][2]uint64{}}
	s.cpuModel, s.cpuThread = cpuInfo(s.path("/proc/cpuinfo"))
	return s
}

func (s *Sampler) path(p string) string { return filepath.Join(s.root, p) }

// Sample takes a new reading (disks maps a label to a path whose filesystem
// is reported) and adds it to the history.
func (s *Sampler) Sample(disks map[string]string) Host {
	return s.sampleAt(time.Now(), disks)
}

func (s *Sampler) sampleAt(now time.Time, disks map[string]string) Host {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := Host{CPUModel: s.cpuModel, CPUThreads: s.cpuThread, Ifaces: []Iface{}, Disks: []DiskInfo{}}
	secs := now.Sub(s.prevAt).Seconds()
	first := s.prevAt.IsZero() || secs <= 0
	rate := func(cur, prev uint64) float64 {
		if first || cur < prev {
			return 0
		}
		return float64(cur-prev) / secs
	}

	if all, idle, ok := cpuTimes(s.path("/proc/stat")); ok {
		if !first && all > s.prevCPU[0] && idle >= s.prevCPU[1] {
			dAll, dIdle := all-s.prevCPU[0], idle-s.prevCPU[1]
			h.CPUPercent = float64(dAll-min(dIdle, dAll)) / float64(dAll) * 100
		}
		s.prevCPU = [2]uint64{all, idle}
	}
	h.CPUTemp = cpuTemp(s.path("/sys/class/hwmon"))
	if b, err := os.ReadFile(s.path("/proc/loadavg")); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 4 {
			h.Load1, _ = strconv.ParseFloat(f[0], 64)
			h.Load5, _ = strconv.ParseFloat(f[1], 64)
			h.Load15, _ = strconv.ParseFloat(f[2], 64)
			if _, total, ok := strings.Cut(f[3], "/"); ok {
				h.Procs, _ = strconv.Atoi(total)
			}
		}
	}
	if b, err := os.ReadFile(s.path("/proc/uptime")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			h.UptimeSec, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	s.memory(&h)

	net := netCounters(s.path("/proc/net/dev"))
	for _, name := range physicalIfaces(s.path("/sys/class/net")) {
		c, ok := net[name]
		if !ok {
			continue
		}
		ifc := Iface{Name: name, Rx: rate(c[0], s.prevNet[name][0]), Tx: rate(c[1], s.prevNet[name][1])}
		if b, err := os.ReadFile(s.path("/sys/class/net/" + name + "/operstate")); err == nil {
			ifc.Up = strings.TrimSpace(string(b)) == "up"
		}
		if b, err := os.ReadFile(s.path("/sys/class/net/" + name + "/speed")); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 && ifc.Up {
				ifc.Speed = n
			}
		}
		h.NetRx += ifc.Rx
		h.NetTx += ifc.Tx
		h.Ifaces = append(h.Ifaces, ifc)
	}
	rates := make(map[string][2]float64, len(net))
	for name, c := range net {
		if p, ok := s.prevNet[name]; ok {
			rates[name] = [2]float64{rate(c[0], p[0]), rate(c[1], p[1])}
		}
	}
	s.rates = rates
	s.prevNet = net

	if rd, wr, ok := diskSectors(s.path("/proc/diskstats"), s.path("/sys/block")); ok {
		h.DiskRead = rate(rd, s.prevDisk[0]) * 512
		h.DiskWrite = rate(wr, s.prevDisk[1]) * 512
		s.prevDisk = [2]uint64{rd, wr}
	}
	h.Disks = statDisks(disks)
	s.prevAt = now

	if !first {
		p := Point{T: now.Unix(), CPU: h.CPUPercent, NetRx: h.NetRx, NetTx: h.NetTx, DiskRead: h.DiskRead, DiskWrite: h.DiskWrite, CPUTemp: h.CPUTemp}
		if h.MemTotal > 0 {
			p.Mem = float64(h.MemUsed) / float64(h.MemTotal) * 100
		}
		s.hist = append(s.hist, p)
		if len(s.hist) > HistoryLen {
			s.hist = s.hist[len(s.hist)-HistoryLen:]
		}
	}
	s.last = h
	return h
}

// Rates are the receive and send rates (bytes/s) of every interface,
// bonds, bridges and VLANs included, from the last two samples.
func (s *Sampler) Rates() map[string][2]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.rates)
}

// Latest returns the newest sample and the history, oldest first.
func (s *Sampler) Latest() (Host, []Point) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, append([]Point{}, s.hist...)
}

// memory fills the memory figures: programs, page cache, ZFS ARC, free.
// MemAvailable doesn't count the ARC as available, so without taking it out
// a ZFS server looks almost full.
func (s *Sampler) memory(h *Host) {
	mem := meminfo(s.path("/proc/meminfo"))
	h.MemTotal = mem["MemTotal"]
	avail, free := mem["MemAvailable"], mem["MemFree"]
	h.MemFree = free
	if avail >= free {
		h.MemCache = avail - free
	}
	used := uint64(0)
	if h.MemTotal >= avail {
		used = h.MemTotal - avail
	}
	if arc := kstat(s.path("/proc/spl/kstat/zfs/arcstats")); arc["size"] > 0 {
		h.MemARC = min(arc["size"], used)
		h.MemARCMax = arc["c_max"]
		used -= h.MemARC
	}
	h.MemUsed = used
	h.SwapTotal = mem["SwapTotal"]
	if h.SwapTotal >= mem["SwapFree"] {
		h.SwapUsed = h.SwapTotal - mem["SwapFree"]
	}
}

// Read returns memory, load and uptime figures and the space on the given
// paths, without the rates (those need a Sampler).
func Read(disks map[string]string) Host {
	return NewSampler("").sampleAt(time.Now(), disks)
}

// ---------- Readers ----------

func cpuInfo(path string) (model string, threads int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "processor":
			threads++
		case "model name":
			if model == "" {
				model = strings.Join(strings.Fields(v), " ")
			}
		}
	}
	return model, threads
}

func cpuTimes(path string) (all, idle uint64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for i, v := range fields[1:] {
		if i >= 8 { // guest time is already counted in user/nice
			break
		}
		n, _ := strconv.ParseUint(v, 10, 64)
		all += n
		if i == 3 || i == 4 { // idle + iowait
			idle += n
		}
	}
	return all, idle, true
}

// cpuTemp reads the CPU package temperature from the hwmon drivers that
// report it (k10temp/zenpower on AMD, coretemp on Intel).
func cpuTemp(hwmon string) *float64 {
	dirs, _ := filepath.Glob(filepath.Join(hwmon, "hwmon*"))
	sort.Strings(dirs)
	prefer := map[string][]string{
		"k10temp": {"Tctl", "Tdie"}, "zenpower": {"Tdie", "Tctl"}, "coretemp": {"Package id 0"}, "cpu_thermal": {""},
	}
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "name"))
		if err != nil {
			continue
		}
		labels, ok := prefer[strings.TrimSpace(string(b))]
		if !ok {
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		sort.Strings(inputs)
		pick := ""
		for _, want := range labels {
			for _, in := range inputs {
				l, _ := os.ReadFile(strings.TrimSuffix(in, "_input") + "_label")
				if want == "" || strings.TrimSpace(string(l)) == want {
					pick = in
					break
				}
			}
			if pick != "" {
				break
			}
		}
		if pick == "" && len(inputs) > 0 {
			pick = inputs[0]
		}
		if v, err := os.ReadFile(pick); err == nil {
			if n, err := strconv.ParseFloat(strings.TrimSpace(string(v)), 64); err == nil && n > 0 {
				t := n / 1000
				return &t
			}
		}
	}
	return nil
}

func meminfo(path string) map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(v)
		if len(fs) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(fs[0], 10, 64)
		out[k] = n * 1024
	}
	return out
}

// kstat reads a ZFS kstat file ("name type value" lines).
func kstat(path string) map[string]uint64 {
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

// netCounters reads received and sent bytes per interface.
func netCounters(path string) map[string][2]uint64 {
	out := map[string][2]uint64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out[strings.TrimSpace(name)] = [2]uint64{rx, tx}
	}
	return out
}

// physicalIfaces lists network ports backed by hardware (they have a
// device), so bonds, bridges, VLANs and container interfaces aren't counted
// twice.
func physicalIfaces(sysNet string) []string {
	des, err := os.ReadDir(sysNet)
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		if _, err := os.Stat(filepath.Join(sysNet, de.Name(), "device")); err == nil {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out
}

// diskSectors adds up sectors read and written on whole physical disks
// (those in /sys/block with a device: not partitions, loop, dm, md or zram).
func diskSectors(diskstats, sysBlock string) (rd, wr uint64, ok bool) {
	b, err := os.ReadFile(diskstats)
	if err != nil {
		return 0, 0, false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		if _, err := os.Stat(filepath.Join(sysBlock, f[2], "device")); err != nil {
			continue
		}
		r, _ := strconv.ParseUint(f[5], 10, 64)
		w, _ := strconv.ParseUint(f[9], 10, 64)
		rd += r
		wr += w
	}
	return rd, wr, true
}

func statDisks(disks map[string]string) []DiskInfo {
	out := []DiskInfo{}
	seen := map[[2]uint64]bool{} // skip paths that are on the same filesystem
	for _, label := range sortedKeys(disks) {
		path := disks[label]
		var st syscall.Statfs_t
		if path == "" || syscall.Statfs(path, &st) != nil {
			continue
		}
		key := [2]uint64{st.Blocks, st.Files}
		if seen[key] {
			continue
		}
		seen[key] = true
		total := st.Blocks * uint64(st.Bsize)
		free := st.Bavail * uint64(st.Bsize)
		out = append(out, DiskInfo{Label: label, Path: path, Total: total, Used: total - free})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
