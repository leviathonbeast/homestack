package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const gb = uint64(1 << 30)

type recordRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}

// arcHost is a fake server with 64 GB of RAM and ZFS loaded. Like the
// kernel, it applies a zfs_arc_max it accepts to arcstats' c_max, and
// ignores one it doesn't (here: anything at or below c_min, and 3 GB).
func arcHost(t *testing.T) (*Agent, string, *recordRunner) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"proc/spl/kstat/zfs", "sys/module/zfs/parameters", "etc/modprobe.d"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	stats := map[string]uint64{"size": 51 * gb, "c": 51 * gb, "c_min": 2 * gb, "c_max": 63 * gb, "hits": 900, "misses": 100, "memory_all_bytes": 64 * gb}
	writeStats := func() {
		var b strings.Builder
		b.WriteString("13 1 0x01 123 33440 1 1\nname                            type data\n")
		for k, v := range stats {
			fmt.Fprintf(&b, "%-32s4    %d\n", k, v)
		}
		os.WriteFile(filepath.Join(root, "proc/spl/kstat/zfs/arcstats"), []byte(b.String()), 0o644)
	}
	writeStats()
	param := func(n, v string) {
		os.WriteFile(filepath.Join(root, "sys/module/zfs/parameters", n), []byte(v+"\n"), 0o644)
	}
	param("zfs_arc_max", "0")
	param("zfs_arc_min", "0")
	os.WriteFile(filepath.Join(root, "etc/modprobe.d/zfs.conf"), []byte("# mine\noptions zfs zfs_prefetch_disable=1\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc/modprobe.d/nvme.conf"), []byte("options nvme_core default_ps_max_latency_us=0\n"), 0o644)

	r := &recordRunner{}
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), Runner: r})
	var mu sync.Mutex
	a.arc.writeSys = func(p string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
		v, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		switch filepath.Base(p) {
		case "zfs_arc_min":
			if v > 0 {
				stats["c_min"] = v
			}
		case "zfs_arc_max":
			switch {
			case v == 0:
				stats["c_max"] = 63 * gb
			case v > stats["c_min"] && v != 3*gb:
				stats["c_max"] = v
				stats["c"] = min(stats["c"], v)
			}
		}
		writeStats()
		return nil
	}
	return a, root, r
}

func TestARCLimit(t *testing.T) {
	a, root, r := arcHost(t)
	conf := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(root, "etc/modprobe.d", name))
		return string(b)
	}

	v := a.arc.view()
	if !v.Available || v.Size != 51*gb || v.Max != 63*gb || v.Limit != 0 || v.MemTotal != 64*gb || v.HitRate != 90 || v.MaxLimit != 63*gb || v.Boot != "" {
		t.Fatalf("view %+v", v)
	}

	// Set: in force now, and saved next to the options already there.
	v, err := a.arc.set(16 * gb)
	if err != nil || v.Max != 16*gb || v.Limit != 16*gb || v.Saved != 16*gb || v.Target != 16*gb {
		t.Fatalf("set 16: %+v %v", v, err)
	}
	if c := conf("zfs.conf"); !strings.Contains(c, "# mine\noptions zfs zfs_prefetch_disable=1\n") || !strings.Contains(c, "options zfs zfs_arc_max=17179869184\n") {
		t.Fatalf("zfs.conf:\n%s", c)
	}
	// Changed again: edited in place, not added twice.
	a.arc.set(8 * gb)
	if c := conf("zfs.conf"); strings.Count(c, "zfs_arc_max=") != 1 || !strings.Contains(c, "zfs_arc_max=8589934592") {
		t.Fatalf("zfs.conf after change:\n%s", c)
	}

	// Refused: too small, too big, and a value ZFS ignores (put back as it was).
	for _, bad := range []uint64{512 << 20, 63*gb + 1} {
		if _, err := a.arc.set(bad); err == nil {
			t.Errorf("%d accepted", bad)
		}
	}
	if _, err := a.arc.set(3 * gb); err == nil || !strings.Contains(err.Error(), "didn't accept") {
		t.Fatalf("ignored value: %v", err)
	}
	if v := a.arc.view(); v.Limit != 8*gb || v.Max != 8*gb {
		t.Fatalf("not restored after a refusal: %+v", v)
	}

	// At or below ZFS's minimum: the minimum is lowered too, first.
	v, err = a.arc.set(2 * gb)
	if err != nil || v.Max != 2*gb || v.Min != 1*gb || !strings.Contains(conf("zfs.conf"), "zfs_arc_min=1073741824") {
		t.Fatalf("set at the minimum: %+v %v\n%s", v, err, conf("zfs.conf"))
	}

	// Back to ZFS's default: our setting and note go, the rest stays.
	v, err = a.arc.set(0)
	if err != nil || v.Limit != 0 || v.Max != 63*gb || v.Saved != 0 {
		t.Fatalf("default: %+v %v", v, err)
	}
	if c := conf("zfs.conf"); strings.Contains(c, "zfs_arc_max") || strings.Contains(c, "Homestack") || !strings.Contains(c, "zfs_prefetch_disable=1") {
		t.Fatalf("zfs.conf after reset:\n%s", c)
	}
	if conf("nvme.conf") != "options nvme_core default_ps_max_latency_us=0\n" {
		t.Fatal("another module's settings changed")
	}
	if len(r.calls) != 0 {
		t.Fatalf("no ZFS in the boot image, but ran %v", r.calls)
	}
}

// The note an earlier version wrote is reworded with the setting, and goes
// with it.
func TestARCLimitOldNote(t *testing.T) {
	a, root, _ := arcHost(t)
	f := filepath.Join(root, "etc/modprobe.d/zfs.conf")
	old := "# mine\n" + oldARCComment + "\noptions zfs zfs_arc_max=8589934592\n"

	os.WriteFile(f, []byte(old), 0o644)
	if _, err := a.arc.set(16 * gb); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "# mine\n"+arcComment+"\noptions zfs zfs_arc_max=17179869184\n" {
		t.Fatalf("zfs.conf after set:\n%s", b)
	}

	os.WriteFile(f, []byte(old), 0o644)
	if _, err := a.arc.set(0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "# mine\n" {
		t.Fatalf("zfs.conf after reset:\n%s", b)
	}
}

func TestARCLimitElsewhereAndBootImage(t *testing.T) {
	a, root, r := arcHost(t)
	os.WriteFile(filepath.Join(root, "etc/modprobe.d/zz-arc.conf"), []byte("options zfs zfs_arc_max=4294967296 zfs_txg_timeout=10\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "usr/share/initramfs-tools/hooks"), 0o755)
	os.WriteFile(filepath.Join(root, "usr/share/initramfs-tools/hooks/zfs"), nil, 0o755)
	if v := a.arc.view(); v.Saved != 4*gb || !strings.HasSuffix(v.SavedIn, "zz-arc.conf") {
		t.Fatalf("existing setting not found: %+v", v)
	}
	if _, err := a.arc.set(12 * gb); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "etc/modprobe.d/zz-arc.conf"))
	if string(b) != "options zfs zfs_arc_max=12884901888 zfs_txg_timeout=10\n" {
		t.Fatalf("zz-arc.conf: %q", b)
	}
	if c, _ := os.ReadFile(filepath.Join(root, "etc/modprobe.d/zfs.conf")); strings.Contains(string(c), "zfs_arc_max") {
		t.Fatal("set twice")
	}
	for range 50 {
		if a.arc.view().Boot == "ok" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 || r.calls[0] != "update-initramfs -u -k all" {
		t.Fatalf("boot image: %v", r.calls)
	}
}
