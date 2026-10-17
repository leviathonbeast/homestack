package libvirt

import (
	"reflect"
	"strings"
	"testing"
)

// Output captured from libvirt 11.3 (Debian 13) with a Debian 13 guest.

func TestParseDomStats(t *testing.T) {
	kv := map[string]string{}
	for line := range strings.SplitSeq(`state.state=1
cpu.time=103630000000
balloon.current=1048576
balloon.unused=817944
balloon.available=984792
balloon.usable=782132
balloon.rss=1563876
vcpu.current=2
vcpu.maximum=2
vcpu.0.time=51060000000
net.count=1
net.0.name=vnet0
net.0.rx.bytes=31620832
net.0.tx.bytes=279305
block.count=2
block.0.name=vda
block.0.path=/srv/dom/web01/vdisk1.qcow2
block.0.rd.bytes=190014976
block.0.wr.bytes=645153280
block.0.allocation=319488000
block.0.capacity=21474836480
block.1.name=sdb
block.1.path=/srv/dom/web01/cloud-init.iso
block.1.rd.bytes=200928
block.1.wr.bytes=0
block.1.allocation=0
block.1.capacity=57344`, "\n") {
		k, v, _ := strings.Cut(line, "=")
		kv[k] = v
	}
	st := parseDomStats(kv)
	want := DomStats{
		State: "running", CPUTime: 103630000000, VCPUs: 2, RSS: 1563876 * 1024, Avail: 984792 * 1024, Usable: 782132 * 1024,
		NetRx: 31620832, NetTx: 279305, DiskRd: 190014976 + 200928, DiskWr: 645153280,
		Disks: []DiskStat{
			{Name: "vda", Path: "/srv/dom/web01/vdisk1.qcow2", Capacity: 21474836480, Allocation: 319488000},
			{Name: "sdb", Path: "/srv/dom/web01/cloud-init.iso", Capacity: 57344},
		},
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got  %+v\nwant %+v", st, want)
	}
	if off := parseDomStats(map[string]string{"state.state": "5"}); off.State != "shutoff" || off.Disks != nil {
		t.Fatalf("stopped VM: %+v", off)
	}
}

func TestParseIfAddr(t *testing.T) {
	agent := ` lo         00:00:00:00:00:00    ipv4         127.0.0.1/8
 -          -                    ipv6         ::1/128
 enp1s0     52:54:00:CE:5f:5d    ipv4         192.168.122.131/24
 -          -                    ipv6         fe80::5054:ff:fece:5f5d/64
 -          -                    ipv6         2001:db8::5/64
 docker0    02:42:ac:11:00:01    ipv4         172.17.0.1/16
 wg0        -                    ipv6         fe80::1/64
`
	got := parseIfAddr(agent)
	want := []IfAddr{
		{Name: "enp1s0", MAC: "52:54:00:ce:5f:5d", Addrs: []string{"192.168.122.131/24", "2001:db8::5/64"}},
		{Name: "docker0", MAC: "02:42:ac:11:00:01", Addrs: []string{"172.17.0.1/16"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agent:\n got %+v\nwant %+v", got, want)
	}
	arp := parseIfAddr(" vnet0      52:54:00:ce:5f:5d    ipv4         192.168.122.131/0\n")
	if len(arp) != 1 || arp[0].Addrs[0] != "192.168.122.131" {
		t.Fatalf("arp: %+v", arp)
	}
	if len(parseIfAddr("")) != 0 {
		t.Fatal("empty output")
	}
}

func TestParseGuestInfo(t *testing.T) {
	out := `os.id               : debian
os.name             : Debian GNU/Linux
os.pretty-name      : Debian GNU/Linux 13 (trixie)
os.version          : 13 (trixie)
os.kernel-release   : 6.12.111+deb13-cloud-amd64
os.kernel-version   : #1 SMP PREEMPT_DYNAMIC Debian 6.12.111-1 (2026-09-28)
timezone.name       : BST
timezone.offset     : 3600
hostname            : web01
fs.count            : 4
fs.0.name           : vda1
fs.0.mountpoint     : /
fs.0.fstype         : ext4
fs.0.total-bytes    : 20020916224
fs.0.used-bytes     : 892141568
fs.1.name           : vda15
fs.1.mountpoint     : /boot/efi
fs.1.fstype         : vfat
fs.1.total-bytes    : 129718272
fs.1.used-bytes     : 9322496
fs.2.mountpoint     : /snap/core/1
fs.2.fstype         : squashfs
fs.2.total-bytes    : 100
fs.2.used-bytes     : 100
fs.3.mountpoint     : /
fs.3.fstype         : ext4
fs.3.total-bytes    : 20020916224
fs.3.used-bytes     : 892141568
`
	g := parseGuestInfo(out)
	want := GuestInfo{OS: "Debian GNU/Linux 13 (trixie)", Kernel: "6.12.111+deb13-cloud-amd64", Hostname: "web01", Timezone: "BST (UTC+1)",
		FS: []GuestFS{{Mount: "/", Type: "ext4", Total: 20020916224, Used: 892141568}, {Mount: "/boot/efi", Type: "vfat", Total: 129718272, Used: 9322496}}}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("got  %+v\nwant %+v", g, want)
	}
	if tz := parseGuestInfo("timezone.name : IST\ntimezone.offset : 19800\n").Timezone; tz != "IST (UTC+5:30)" {
		t.Fatalf("tz %q", tz)
	}
	if tz := parseGuestInfo("timezone.name : NDT\ntimezone.offset : -9000\n").Timezone; tz != "NDT (UTC-2:30)" {
		t.Fatalf("tz %q", tz)
	}
}
