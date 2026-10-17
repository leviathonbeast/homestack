package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// procState is a process's state letter from /proc (T: stopped).
func procState(pid int) byte {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	i := bytes.LastIndexByte(b, ')')
	if err != nil || i < 0 || i+2 >= len(b) {
		return 0
	}
	return b[i+2]
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting until %s", what)
}

// Pause stops a parity job's processes where they are, Resume lets them
// carry on, a timed pause ends by itself, and Stop still works while paused.
func TestPauseResume(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "job")
	os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	a := New(Options{StateDir: dir, LogDir: dir, Runner: &scriptRunner{out: map[string]string{}}, LockFile: filepath.Join(dir, "lock")})
	m := a.jobs
	if err := m.pause(0); err == nil {
		t.Error("paused with nothing running")
	}
	if _, err := m.runCommand("parity-sync", "Parity sync", script); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	pid := m.proc.Process.Pid
	m.mu.Unlock()
	running := func() bool { s := procState(pid); return s == 'S' || s == 'R' }
	until(t, "it runs", running)

	if err := m.pause(0); err != nil {
		t.Fatal(err)
	}
	until(t, "it's paused", func() bool { return procState(pid) == 'T' })
	if _, r := m.snapshot(); r.Paused == nil || r.ResumeAt != nil || r.Speed != "" {
		t.Errorf("paused job: %+v", r)
	}
	if _, err := a.startAction(context.Background(), ActionRequest{Action: "mover"}); err == nil || !strings.Contains(err.Error(), "Parity sync is paused") {
		t.Errorf("starting the mover while paused: %v", err)
	}
	if err := m.resume(); err != nil {
		t.Fatal(err)
	}
	until(t, "it runs again", running)
	if m.pausedTitle() != "" {
		t.Error("still marked paused")
	}

	// A timed pause carries on by itself.
	if err := m.pause(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, r := m.snapshot(); r.ResumeAt == nil {
		t.Errorf("no resume time: %+v", r)
	}
	until(t, "the timed pause ends", func() bool { return m.pausedTitle() == "" && running() })

	// Stopping a paused job: it gets the SIGINT once it runs again.
	if err := m.pause(0); err != nil {
		t.Fatal(err)
	}
	until(t, "it's paused again", func() bool { return procState(pid) == 'T' })
	if err := m.stop(); err != nil {
		t.Fatal(err)
	}
	until(t, "it stops", func() bool { _, r := m.snapshot(); return r == nil })
	until(t, "the status is refreshed", func() bool { return !a.Snapshot().Updated.IsZero() })
	if h, _ := m.snapshot(); h[0].State != "stopped" || h[0].Paused != nil {
		t.Errorf("after stopping: %+v", h[0])
	}

	// Only parity jobs pause.
	m.mu.Lock()
	m.running = &Job{ID: "x", Action: "mover", Title: "Mover"}
	m.mu.Unlock()
	if err := m.pause(0); err == nil || !strings.Contains(err.Error(), "can't be paused") {
		t.Errorf("paused the mover: %v", err)
	}
}

// TRIM, with lucy's own output: each SSD filesystem in turn, the ZFS pools
// on hard disks left out, a pool on an SSD trimmed, and a pool whose disk
// turns out not to support it isn't a failure.
func TestTrim(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "dev/disk/by-id"), 0o755)
	os.WriteFile(filepath.Join(root, "dev/sdb"), nil, 0o644)
	os.Symlink("../../sdb", filepath.Join(root, "dev/disk/by-id/ata-ST16000NE000-2RW103_ZL2PR26D"))
	r := &scriptRunner{out: map[string]string{
		"fstrim --all --dry-run": "/mnt/disks/242895D00061: 0 B (dry run) trimmed on /dev/sdf1\n/mnt/cache: 0 B (dry run) trimmed on /dev/nvme0n1p1\n/: 0 B (dry run) trimmed on /dev/mapper/lucy--vg-root\n",
		"fstrim --verbose --quiet-unsupported /mnt/disks/242895D00061": "/mnt/disks/242895D00061: 1.8 TiB (2000397860864 bytes) trimmed\n",
		"fstrim --verbose --quiet-unsupported /mnt/cache":              "/mnt/cache: 287.4 GiB (308607406080 bytes) trimmed\n",
		"fstrim --verbose --quiet-unsupported /":                       "/: 2.7 GiB (2897534976 bytes) trimmed\n",
		"zpool trim -w fast":                                           "",
		"zpool trim -w usb":                                            "!cannot trim '/dev/sdz1': trim operations are not supported by this device\n",
	}}
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), Runner: r})
	a.disks.disks = []Disk{{Name: "sdb", Rotational: true}, {Name: "nvme0n1"}, {Name: "sdf"}}
	pools := []Pool{
		{Name: "disk1", Devices: []string{"/dev/disk/by-id/ata-ST16000NE000-2RW103_ZL2PR26D-part1"}},
		{Name: "fast", Devices: []string{"/dev/nvme0n1p3"}},
		{Name: "usb", Devices: []string{"/dev/sdz1"}}, // a disk it doesn't know: tried
	}
	var log []string
	logf := func(s string) { log = append(log, s) }
	sum, err := a.trim(context.Background(), pools, logf)
	if err != nil || sum != "Trimmed /mnt/disks/242895D00061 (1.8 TiB), /mnt/cache (287.4 GiB), / (2.7 GiB), pool fast." {
		t.Errorf("%q %v", sum, err)
	}
	text := strings.Join(log, "\n")
	for _, want := range []string{"Trimming /mnt/cache (2 of 3)\n  287.4 GiB trimmed in", "Not trimming the ZFS pools on hard disks: disk1.", "usb (zpool trim)\n  Its disks don't support TRIM."} {
		if !strings.Contains(text, want) {
			t.Errorf("log lacks %q:\n%s", want, text)
		}
	}
	if slices.Contains(r.ran, "zpool trim -w disk1") {
		t.Error("tried to trim a pool on a hard disk")
	}

	// A real failure fails the job; Stop stops it.
	r.out["zpool trim -w fast"] = "!cannot trim 'fast': pool I/O is currently suspended\n"
	if _, err := a.trim(context.Background(), pools[1:2], logf); err == nil || !strings.Contains(err.Error(), "Failed: fast: cannot trim 'fast': pool I/O is currently suspended") {
		t.Errorf("a failed pool: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.trim(ctx, pools, logf); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("stopped: %v", err)
	}

	// An fstrim whose dry run it can't read: everything in one go.
	r.out["fstrim --all --dry-run"] = "/mnt/cache: something else\n"
	r.out["fstrim --all --verbose"] = "/mnt/cache: 1 GiB (1073741824 bytes) trimmed on /dev/nvme0n1p1\n"
	if sum, err := a.trim(context.Background(), nil, logf); err != nil || sum != "Trimmed /mnt/cache (1 GiB)." {
		t.Errorf("fallback: %q %v", sum, err)
	}
}
