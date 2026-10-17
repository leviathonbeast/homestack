package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"homestack/internal/libvirt"
)

// Live figures for the VM pages. Every 10 seconds one `virsh domstats` call
// reads every VM's counters, which become CPU, memory, network and disk
// rates with 15 minutes of history for the charts; it's cheap enough to run
// all the time, and stays quiet on hosts without libvirt. Addresses and
// what the guest agent reports are fetched every 30 seconds, only while a
// VM page is open.

const (
	vmEvery    = 10 * time.Second
	vmPoints   = 90 // 15 minutes
	guestEvery = 30 * time.Second
	guestIdle  = 3 * time.Minute
)

// vmPoint is one reading for the charts. Rates are per second.
type vmPoint struct {
	T         int64   `json:"t"`
	CPU       float64 `json:"cpu"` // percent of its vCPUs
	Mem       uint64  `json:"mem"` // bytes in use (see memSource)
	NetRx     float64 `json:"netRx"`
	NetTx     float64 `json:"netTx"`
	DiskRead  float64 `json:"diskRead"`
	DiskWrite float64 `json:"diskWrite"`
}

type vmTrack struct {
	last   libvirt.DomStats
	lastAt time.Time
	points []vmPoint
}

// vmGuest is what's known about a VM from the inside.
type vmGuest struct {
	Checked time.Time          `json:"checked"`
	Source  string             `json:"source"` // agent | lease | arp | ""
	Ifaces  []libvirt.IfAddr   `json:"ifaces"`
	Info    *libvirt.GuestInfo `json:"info,omitempty"`
}

type vmStats struct {
	mu     sync.Mutex
	track  map[string]*vmTrack
	guest  map[string]*vmGuest
	wanted time.Time
	wake   chan struct{}
}

func newVMStats() *vmStats {
	return &vmStats{track: map[string]*vmTrack{}, guest: map[string]*vmGuest{}, wake: make(chan struct{}, 1)}
}

// want says a VM page is open: keep addresses and guest details fresh.
func (v *vmStats) want() {
	v.mu.Lock()
	idle := time.Since(v.wanted) > guestIdle
	v.wanted = time.Now()
	v.mu.Unlock()
	if idle {
		select {
		case v.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Server) sampleVMs(ctx context.Context) {
	var lastGuest time.Time
	for {
		wait := vmEvery
		if err := s.readVMStats(ctx); errors.Is(err, libvirt.ErrUnavailable) {
			wait = time.Minute // no libvirt here (yet): look now and then
		}
		s.vmstats.mu.Lock()
		active := time.Since(s.vmstats.wanted) < guestIdle
		s.vmstats.mu.Unlock()
		if active && time.Since(lastGuest) >= guestEvery {
			s.readGuests(ctx)
			lastGuest = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-s.vmstats.wake:
			s.readGuests(ctx)
			lastGuest = time.Now()
		case <-time.After(wait):
		}
	}
}

func (s *Server) readVMStats(ctx context.Context) error {
	lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	all, err := s.vms.Stats(lctx)
	if err != nil {
		return err
	}
	now := time.Now()
	v := s.vmstats
	v.mu.Lock()
	defer v.mu.Unlock()
	for name := range v.track {
		if st, ok := all[name]; !ok || st.State != "running" {
			delete(v.track, name) // gone or stopped: the charts start again
		}
	}
	for name := range v.guest {
		if st, ok := all[name]; !ok || st.State != "running" {
			delete(v.guest, name)
		}
	}
	for name, st := range all {
		if st.State != "running" {
			continue
		}
		t := v.track[name]
		if t == nil {
			v.track[name] = &vmTrack{last: st, lastAt: now}
			continue
		}
		if p, ok := rates(t.last, st, now.Sub(t.lastAt)); ok {
			p.T = now.Unix()
			t.points = append(t.points, p)
			if len(t.points) > vmPoints {
				t.points = slices.Delete(t.points, 0, len(t.points)-vmPoints)
			}
		}
		t.last, t.lastAt = st, now
	}
	return nil
}

// rates turns two readings into a chart point. A gap (Homestack was busy,
// or the VM restarted in between) gives no point rather than a wrong one.
func rates(a, b libvirt.DomStats, dt time.Duration) (vmPoint, bool) {
	if dt <= 0 || dt > 4*vmEvery || b.CPUTime < a.CPUTime || b.NetRx < a.NetRx || b.NetTx < a.NetTx || b.DiskRd < a.DiskRd || b.DiskWr < a.DiskWr {
		return vmPoint{}, false
	}
	sec := dt.Seconds()
	p := vmPoint{
		NetRx: float64(b.NetRx-a.NetRx) / sec, NetTx: float64(b.NetTx-a.NetTx) / sec,
		DiskRead: float64(b.DiskRd-a.DiskRd) / sec, DiskWrite: float64(b.DiskWr-a.DiskWr) / sec,
		Mem: b.RSS,
	}
	if b.VCPUs > 0 {
		p.CPU = min(100, float64(b.CPUTime-a.CPUTime)/float64(dt.Nanoseconds())/float64(b.VCPUs)*100)
	}
	if b.Avail > 0 && b.Usable > 0 && b.Usable <= b.Avail {
		p.Mem = b.Avail - b.Usable // what programs in the VM use, without its caches
	}
	return p, true
}

// readGuests refreshes the addresses (and agent details) of running VMs, a
// few at a time.
func (s *Server) readGuests(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	vms, err := s.vms.List(lctx)
	if err != nil {
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, vm := range vms {
		if vm.State != "running" || len(vm.NICs) == 0 && vm.Agent != "connected" {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			g := s.guestOf(lctx, vm)
			s.vmstats.mu.Lock()
			s.vmstats.guest[vm.Name] = g
			s.vmstats.mu.Unlock()
		})
	}
	wg.Wait()
}

func (s *Server) guestOf(ctx context.Context, vm libvirt.VM) *vmGuest {
	g := &vmGuest{Checked: time.Now(), Ifaces: []libvirt.IfAddr{}}
	if vm.Agent == "connected" {
		actx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		if ifs, err := s.vms.Addresses(actx, vm.Name, "agent"); err == nil {
			g.Source, g.Ifaces = "agent", ifs
		}
		if info, err := s.vms.GuestInfo(actx, vm.Name); err == nil {
			g.Info = &info
		}
		if g.Source != "" {
			return g
		}
	}
	// No agent: what the host knows. DHCP leases for VMs on a libvirt
	// network, the ARP table for VMs on a bridge.
	for _, src := range []string{"lease", "arp"} {
		ifs, err := s.vms.Addresses(ctx, vm.Name, src)
		if err != nil {
			continue
		}
		for _, i := range ifs {
			if !slices.ContainsFunc(g.Ifaces, func(x libvirt.IfAddr) bool { return x.MAC == i.MAC }) {
				g.Ifaces = append(g.Ifaces, i)
				g.Source = firstNonEmptyStr(g.Source, src)
			}
		}
	}
	return g
}

// fill adds what the sampler knows to a VM from the list: its addresses, and
// the memory programs in it use when its balloon driver reports that (the
// process size libvirt gives otherwise includes the VM's caches and QEMU's
// own overhead, so it can read more than the VM has).
func (v *vmStats) fill(vm *libvirt.VM) {
	vm.Addrs = v.addrsOf(vm.Name)
	v.mu.Lock()
	defer v.mu.Unlock()
	if t := v.track[vm.Name]; t != nil && vm.State == "running" && t.last.Avail > 0 && t.last.Usable > 0 && t.last.Usable <= t.last.Avail {
		vm.MemoryUsed = t.last.Avail - t.last.Usable
		vm.GuestMem = true
	}
}

// addrsOf lists a VM's known addresses, IPv4 first, without prefix lengths.
func (v *vmStats) addrsOf(name string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	g := v.guest[name]
	if g == nil {
		return nil
	}
	var v4, v6 []string
	for _, i := range g.Ifaces {
		for _, a := range i.Addrs {
			ip, _, _ := strings.Cut(a, "/")
			if strings.Contains(ip, ":") {
				v6 = append(v6, ip)
			} else {
				v4 = append(v4, ip)
			}
		}
	}
	return append(v4, v6...)
}

func (s *Server) vmMetrics(w http.ResponseWriter, r *http.Request) {
	s.vmstats.want()
	name := r.PathValue("name")
	v := s.vmstats
	v.mu.Lock()
	res := map[string]any{"interval": int(vmEvery.Seconds()), "history": []vmPoint{}, "disks": []libvirt.DiskStat{}, "guestMem": false}
	if t := v.track[name]; t != nil {
		res["history"] = slices.Clone(t.points)
		if t.last.Disks != nil {
			res["disks"] = slices.Clone(t.last.Disks)
		}
		res["guestMem"] = t.last.Avail > 0 && t.last.Usable > 0
	}
	if g := v.guest[name]; g != nil {
		res["guest"] = g
	}
	v.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}
