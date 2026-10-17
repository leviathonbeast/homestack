package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Disk is one physical disk, with the SMART values that matter.
type Disk struct {
	Name         string    `json:"name"` // sdc, nvme0n1
	Label        string    `json:"label"`
	Role         string    `json:"role"` // pool name, cache, parity, system, or ""
	Model        string    `json:"model"`
	Serial       string    `json:"serial"`
	Size         uint64    `json:"size"`
	Rotational   bool      `json:"rotational"`
	Transport    string    `json:"transport"`
	Temp         *int      `json:"temp"`
	Asleep       bool      `json:"asleep"`
	Health       string    `json:"health"` // ok, failed, unknown
	PowerOnHours *int      `json:"powerOnHours,omitempty"`
	Reallocated  *int64    `json:"reallocated,omitempty"`
	Pending      *int64    `json:"pending,omitempty"`
	Offline      *int64    `json:"offline,omitempty"`
	CRC          *int64    `json:"crc,omitempty"`
	MediaErrors  *int64    `json:"mediaErrors,omitempty"`
	WearUsed     *int      `json:"wearUsed,omitempty"`
	Checked      time.Time `json:"checked"`
	// Reads and writes (io.go): bytes/s now, bytes since boot, and how
	// long it has done nothing.
	ReadBps      float64 `json:"readBps"`
	WriteBps     float64 `json:"writeBps"`
	ReadBytes    uint64  `json:"readBytes"`
	WrittenBytes uint64  `json:"writtenBytes"`
	IdleSec      *int64  `json:"idleSec,omitempty"`
}

type diskPoller struct {
	a     *Agent
	mu    sync.RWMutex
	disks []Disk
	every time.Duration
}

func newDiskPoller(a *Agent) *diskPoller { return &diskPoller{a: a, every: time.Minute} }

func (p *diskPoller) snapshot() []Disk {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Disk(nil), p.disks...)
}

// setAsleep marks a disk asleep (or awake) until SMART next looks.
func (p *diskPoller) setAsleep(name string, asleep bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.disks {
		if p.disks[i].Name == name {
			p.disks[i].Asleep = asleep
		}
	}
}

func (p *diskPoller) loop(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

type lsblkDev struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Tran   string `json:"tran"`
	Rota   any    `json:"rota"`
	Model  string `json:"model"`
	Serial string `json:"serial"`
	Size   any    `json:"size"`
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "1" || t == "true"
	case float64:
		return t == 1
	}
	return false
}

func asUint(v any) uint64 {
	switch t := v.(type) {
	case float64:
		return uint64(t)
	case string:
		var n uint64
		for _, c := range t {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + uint64(c-'0')
		}
		return n
	}
	return 0
}

// roles maps a disk name (sdc) to its role: the pool it belongs to, cache,
// parity or system.
func (a *Agent) roles(ctx context.Context) map[string]string {
	out := map[string]string{}
	parent := func(dev string) string {
		o, err := a.cmd(ctx, 5*time.Second, "lsblk", "-no", "PKNAME", dev)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(strings.SplitN(o, "\n", 2)[0])
	}
	if names, err := a.cmd(ctx, 10*time.Second, "zpool", "list", "-Ho", "name"); err == nil {
		for pool := range strings.FieldsSeq(names) {
			o, err := a.cmd(ctx, 10*time.Second, "zpool", "list", "-vHP", pool)
			if err != nil {
				continue
			}
			for line := range strings.SplitSeq(o, "\n") {
				f := strings.Fields(line)
				if len(f) > 0 && strings.HasPrefix(f[0], "/dev/") {
					if p := parent(f[0]); p != "" {
						out[p] = pool
					}
				}
			}
		}
	}
	l := a.layout()
	mounts := [][2]string{{l.Cache, "cache"}, {l.ParityPath(1), "parity"}}
	seen := map[string]bool{l.ParityPath(1): true}
	if conf, err := readSnapConf(a.snapraidConf()); err == nil {
		for _, p := range conf.parity {
			if m := parentMount(p.Path); !seen[m] {
				seen[m] = true
				mounts = append(mounts, [2]string{m, "parity"})
			}
		}
		// Data disks that aren't ZFS pools (ext4, XFS…): by the device mounted
		// at their path. Their role is the path's name (disk3), like a pool's.
		for _, p := range dataPaths(conf, "") {
			o, err := a.cmd(ctx, 5*time.Second, "findmnt", "-no", "SOURCE,FSTYPE", "--mountpoint", p)
			f := strings.Fields(o)
			if err != nil || len(f) < 2 || f[1] == "zfs" || !strings.HasPrefix(f[0], "/dev/") || strings.HasPrefix(f[0], "/dev/mapper/") {
				continue
			}
			disk := parent(f[0])
			if disk == "" { // a filesystem on the whole disk
				disk = filepath.Base(f[0])
			}
			out[disk] = filepath.Base(p)
		}
	}
	// A parity disk being added or swapped in isn't in snapraid.conf yet
	// (assign.go), but it mustn't look like a spare disk meanwhile.
	for i := 1; i <= maxParity; i++ {
		for _, m := range []string{l.ParityPath(i), l.ParityPath(i) + "-new"} {
			if _, err := os.Stat(a.sys(m)); err == nil && !seen[m] {
				seen[m] = true
				mounts = append(mounts, [2]string{m, "parity"})
			}
		}
	}
	mounts = append(mounts, [2]string{"/", "system"})
	for _, m := range mounts {
		src, err := a.cmd(ctx, 5*time.Second, "findmnt", "-no", "SOURCE", m[0])
		if err != nil {
			continue
		}
		src = strings.TrimSpace(strings.SplitN(src, "[", 2)[0])
		if strings.HasPrefix(src, "/dev/mapper/") || strings.HasPrefix(src, "/dev/dm-") {
			// LVM: find the physical volume behind it
			if pv, err := a.cmd(ctx, 5*time.Second, "pvs", "--noheadings", "-o", "pv_name"); err == nil {
				for d := range strings.FieldsSeq(pv) {
					if p := parent(d); p != "" {
						out[p] = m[1]
					}
				}
			}
			continue
		}
		if p := parent(src); p != "" {
			out[p] = m[1]
		}
	}
	return out
}

func (p *diskPoller) poll(ctx context.Context) {
	a := p.a
	o, err := a.cmd(ctx, 10*time.Second, "lsblk", "-J", "-b", "-d", "-o", "NAME,TYPE,TRAN,ROTA,MODEL,SERIAL,SIZE")
	if err != nil {
		return
	}
	var ls struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if json.Unmarshal([]byte(o), &ls) != nil {
		return
	}
	roles := a.roles(ctx)
	var out []Disk
	for _, d := range ls.Blockdevices {
		if d.Type != "disk" || d.Tran == "usb" || strings.HasPrefix(d.Name, "zd") || strings.HasPrefix(d.Name, "loop") {
			continue
		}
		disk := Disk{
			Name: d.Name, Role: roles[d.Name], Model: strings.TrimSpace(d.Model), Serial: strings.TrimSpace(d.Serial),
			Size: asUint(d.Size), Rotational: truthy(d.Rota), Transport: d.Tran, Health: "unknown", Checked: time.Now(),
		}
		disk.Label = disk.Role
		if disk.Label == "" {
			disk.Label = d.Name
		}
		p.smart(ctx, &disk)
		if disk.Asleep {
			// SMART can't be read without waking it: keep what it said last.
			for _, prev := range p.snapshot() {
				if prev.Name == disk.Name && prev.Serial == disk.Serial {
					disk.Health, disk.PowerOnHours, disk.Reallocated, disk.Pending = prev.Health, prev.PowerOnHours, prev.Reallocated, prev.Pending
					disk.Offline, disk.CRC, disk.MediaErrors, disk.WearUsed = prev.Offline, prev.CRC, prev.MediaErrors, prev.WearUsed
				}
			}
		}
		out = append(out, disk)
	}
	sort.Slice(out, func(i, j int) bool { return diskOrder(out[i]) < diskOrder(out[j]) })
	p.mu.Lock()
	p.disks = out
	p.mu.Unlock()
}

func diskOrder(d Disk) string {
	rank := "3"
	switch {
	case d.Role == "parity":
		rank = "0"
	case strings.HasPrefix(d.Role, "disk"):
		rank = "1"
	case d.Role == "cache":
		rank = "2"
	case d.Role == "system":
		rank = "4"
	}
	// disk2 before disk10
	num := strings.TrimLeft(d.Role, "abcdefghijklmnopqrstuvwxyz")
	return rank + strings.Repeat("0", 4-min(4, len(num))) + num + d.Name
}

type smartJSON struct {
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	ATA *struct {
		Table []struct {
			ID    int    `json:"id"`
			Name  string `json:"name"`
			Value int    `json:"value"`
			Raw   struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMe *struct {
		MediaErrors    int64 `json:"media_errors"`
		PercentageUsed int   `json:"percentage_used"`
	} `json:"nvme_smart_health_information_log"`
	DevStat *struct {
		Pages []struct {
			Table []struct {
				Name  string `json:"name"`
				Value int    `json:"value"`
			} `json:"table"`
		} `json:"pages"`
	} `json:"ata_device_statistics"`
}

// smart reads SMART without waking a sleeping disk (-n standby).
func (p *diskPoller) smart(ctx context.Context, d *Disk) {
	args := []string{"-n", "standby", "-H", "-A", "-j"}
	if !strings.HasPrefix(d.Name, "nvme") {
		args = append(args, "-l", "devstat")
	}
	out, _ := p.a.cmd(ctx, 20*time.Second, "smartctl", append(args, filepath.Join("/dev", d.Name))...)
	var s smartJSON
	if json.Unmarshal([]byte(out), &s) != nil || s.SmartStatus == nil {
		if strings.Contains(out, "STANDBY") || strings.Contains(out, "standby") {
			d.Asleep = true
		}
		return
	}
	d.Health = "ok"
	if !s.SmartStatus.Passed {
		d.Health = "failed"
	}
	if s.Temperature != nil && s.Temperature.Current > 0 {
		t := s.Temperature.Current
		d.Temp = &t
	}
	if s.PowerOnTime != nil {
		h := s.PowerOnTime.Hours
		d.PowerOnHours = &h
	}
	if s.ATA != nil {
		for _, at := range s.ATA.Table {
			v := at.Raw.Value
			switch at.ID {
			case 5:
				d.Reallocated = &v
			case 197:
				d.Pending = &v
			case 198:
				d.Offline = &v
			case 199:
				d.CRC = &v
			}
			if !d.Rotational && d.WearUsed == nil {
				switch at.Name {
				case "Wear_Leveling_Count", "Percent_Lifetime_Remain", "SSD_Life_Left", "Media_Wearout_Indicator", "Percent_Life_Remaining", "Remaining_Lifetime_Perc":
					if at.Value <= 100 {
						w := 100 - at.Value
						d.WearUsed = &w
					}
				}
			}
		}
	}
	if s.DevStat != nil {
		for _, pg := range s.DevStat.Pages {
			for _, e := range pg.Table {
				if e.Name == "Percentage Used Endurance Indicator" {
					w := e.Value
					d.WearUsed = &w
				}
			}
		}
	}
	if s.NVMe != nil {
		m := s.NVMe.MediaErrors
		d.MediaErrors = &m
		w := s.NVMe.PercentageUsed
		d.WearUsed = &w
	}
}
