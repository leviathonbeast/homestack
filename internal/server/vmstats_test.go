package server

import (
	"testing"
	"time"

	"homestack/internal/libvirt"
)

func TestVMRates(t *testing.T) {
	a := libvirt.DomStats{CPUTime: 10e9, VCPUs: 2, RSS: 2 << 30, NetRx: 1000, NetTx: 500, DiskRd: 0, DiskWr: 4096}
	b := a
	b.CPUTime += 5e9 // 5 s of CPU over 10 s on 2 vCPUs = 25%
	b.NetRx += 10_000
	b.DiskWr += 1 << 20
	p, ok := rates(a, b, 10*time.Second)
	if !ok || p.CPU != 25 || p.NetRx != 1000 || p.NetTx != 0 || p.DiskWrite != float64(1<<20)/10 || p.Mem != 2<<30 {
		t.Fatalf("got %+v %v", p, ok)
	}
	// With the balloon driver reporting, memory is what programs in the VM use.
	b.Avail, b.Usable = 1000<<20, 600<<20
	if p, _ := rates(a, b, 10*time.Second); p.Mem != 400<<20 {
		t.Fatalf("guest memory %d", p.Mem)
	}
	// CPU can't read over 100% (timer jitter).
	b.CPUTime = a.CPUTime + 30e9
	if p, _ := rates(a, b, 10*time.Second); p.CPU != 100 {
		t.Fatalf("cpu %v", p.CPU)
	}
	// Counters going backwards (the VM restarted) or a long gap give no point.
	r := b
	r.NetRx = 5
	if _, ok := rates(a, r, 10*time.Second); ok {
		t.Fatal("a restart must not make a point")
	}
	if _, ok := rates(a, b, 5*time.Minute); ok {
		t.Fatal("a long gap must not make a point")
	}
}

func TestAddrsOf(t *testing.T) {
	v := newVMStats()
	v.guest["web"] = &vmGuest{Ifaces: []libvirt.IfAddr{
		{Name: "enp1s0", Addrs: []string{"2001:db8::5/64", "192.168.1.50/24"}},
		{Name: "docker0", Addrs: []string{"172.17.0.1/16"}},
	}}
	got := v.addrsOf("web")
	if len(got) != 3 || got[0] != "192.168.1.50" || got[1] != "172.17.0.1" || got[2] != "2001:db8::5" {
		t.Fatalf("got %v (IPv4 first, no prefix lengths)", got)
	}
	if v.addrsOf("other") != nil {
		t.Fatal("unknown VM")
	}
}
