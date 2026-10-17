package sysinfo

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func write(t *testing.T, root, p, text string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSampler(t *testing.T) {
	root := t.TempDir()
	write(t, root, "/proc/cpuinfo", "processor\t: 0\nmodel name\t: AMD Ryzen 7 5700G with Radeon Graphics\nprocessor\t: 1\nmodel name\t: AMD Ryzen 7 5700G with Radeon Graphics\n")
	write(t, root, "/proc/loadavg", "0.50 0.40 0.30 2/612 12345\n")
	write(t, root, "/proc/uptime", "3600.5 7000.0\n")
	// 32 GiB total, 4 GiB free, 20 GiB available (16 GiB of cache), ARC 8 GiB.
	write(t, root, "/proc/meminfo", "MemTotal: 33554432 kB\nMemFree: 4194304 kB\nMemAvailable: 20971520 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	write(t, root, "/proc/spl/kstat/zfs/arcstats", "13 1 0x01 123 33440 1 1\nname type data\nsize 4 8589934592\nc_max 4 16777216000\n")
	// hwmon: an NVMe sensor first, then the CPU with Tctl on temp1.
	write(t, root, "/sys/class/hwmon/hwmon0/name", "nvme\n")
	write(t, root, "/sys/class/hwmon/hwmon0/temp1_input", "38850\n")
	write(t, root, "/sys/class/hwmon/hwmon1/name", "k10temp\n")
	write(t, root, "/sys/class/hwmon/hwmon1/temp1_input", "52125\n")
	write(t, root, "/sys/class/hwmon/hwmon1/temp1_label", "Tctl\n")
	write(t, root, "/sys/class/hwmon/hwmon1/temp3_input", "48000\n")
	write(t, root, "/sys/class/hwmon/hwmon1/temp3_label", "Tccd1\n")
	// eth0 is a real port; br0 (a bridge) and lo aren't counted.
	write(t, root, "/sys/class/net/eth0/device/vendor", "0x8086\n")
	write(t, root, "/sys/class/net/eth0/operstate", "up\n")
	write(t, root, "/sys/class/net/eth0/speed", "10000\n")
	write(t, root, "/sys/class/net/br0/operstate", "up\n")
	// sda is a disk; sda1 (a partition) and loop0 aren't counted.
	write(t, root, "/sys/block/sda/device/model", "x\n")
	write(t, root, "/sys/block/loop0/size", "0\n")

	stat := func(user, idle int) string {
		return "cpu  " + itoa(user) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0\n"
	}
	netdev := func(rx, tx int) string {
		return "Inter-|   Receive\n face |bytes\n    lo: 999 0 0 0 0 0 0 0 999 0 0 0 0 0 0 0\n  eth0: " + itoa(rx) + " 0 0 0 0 0 0 0 " + itoa(tx) + " 0 0 0 0 0 0 0\n   br0: 5000 0 0 0 0 0 0 0 5000 0 0 0 0 0 0 0\n"
	}
	diskstats := func(rd, wr int) string {
		return "   8       0 sda 0 0 " + itoa(rd) + " 0 0 0 " + itoa(wr) + " 0 0 0 0\n   8       1 sda1 0 0 777 0 0 0 777 0 0 0 0\n   7       0 loop0 0 0 555 0 0 0 555 0 0 0 0\n"
	}
	write(t, root, "/proc/stat", stat(100, 900))
	write(t, root, "/proc/net/dev", netdev(1000, 2000))
	write(t, root, "/proc/diskstats", diskstats(0, 0))

	s := NewSampler(root)
	t0 := time.Unix(1_800_000_000, 0)
	first := s.sampleAt(t0, nil)
	if first.CPUPercent != 0 || first.NetRx != 0 {
		t.Fatalf("first sample has rates: %+v", first)
	}
	if _, hist := s.Latest(); len(hist) != 0 {
		t.Fatal("first sample went into the history")
	}

	write(t, root, "/proc/stat", stat(400, 1600)) // +300 busy of +1000
	write(t, root, "/proc/net/dev", netdev(1000+50_000, 2000+10_000))
	write(t, root, "/proc/diskstats", diskstats(2048, 4096)) // sectors
	h := s.sampleAt(t0.Add(5*time.Second), map[string]string{})

	if h.CPUModel != "AMD Ryzen 7 5700G with Radeon Graphics" || h.CPUThreads != 2 {
		t.Errorf("cpu info %q %d", h.CPUModel, h.CPUThreads)
	}
	if h.CPUPercent != 30 {
		t.Errorf("cpu %% = %v", h.CPUPercent)
	}
	if h.CPUTemp == nil || *h.CPUTemp != 52.125 {
		t.Errorf("cpu temp %v", h.CPUTemp)
	}
	if h.Load1 != 0.5 || h.Procs != 612 || h.UptimeSec != 3600.5 {
		t.Errorf("load/procs/uptime %+v", h)
	}
	const gib = 1 << 30
	if h.MemTotal != 32*gib || h.MemFree != 4*gib || h.MemCache != 16*gib || h.MemARC != 8*gib || h.MemUsed != 4*gib {
		t.Errorf("memory: used %d cache %d arc %d free %d", h.MemUsed/gib, h.MemCache/gib, h.MemARC/gib, h.MemFree/gib)
	}
	if len(h.Ifaces) != 1 || h.Ifaces[0].Name != "eth0" || h.Ifaces[0].Speed != 10000 || h.NetRx != 10_000 || h.NetTx != 2_000 {
		t.Errorf("network %+v rx %v tx %v", h.Ifaces, h.NetRx, h.NetTx)
	}
	if h.DiskRead != 2048.0*512/5 || h.DiskWrite != 4096.0*512/5 {
		t.Errorf("disk io %v %v", h.DiskRead, h.DiskWrite)
	}
	if _, hist := s.Latest(); len(hist) != 1 || hist[0].CPU != 30 || hist[0].Mem != 12.5 {
		t.Errorf("history %+v", hist)
	}

	// Counters that go backwards (a reset) don't make a huge spike.
	write(t, root, "/proc/net/dev", netdev(10, 10))
	if h := s.sampleAt(t0.Add(10*time.Second), nil); h.NetRx != 0 {
		t.Errorf("rate after a counter reset: %v", h.NetRx)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
