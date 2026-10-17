package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Before running SnapRAID, its own ZFS snapshot mounts are unmounted (and
// nothing else): on the host, and a copy a container kept (once per mount
// namespace).
func TestReleaseSnapshots(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	write := func(rel, text string) {
		p := filepath.Join(proc, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	link := func(rel, ns string) {
		p := filepath.Join(proc, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.Symlink(ns, p)
	}
	host := `disk1 /mnt/disk1 zfs rw,xattr,noacl 0 0
disk5 /mnt/disk5 zfs rw,xattr,noacl 0 0
disk5@snapraid-scan /mnt/disk5/.zfs/snapshot/snapraid-scan zfs ro,relatime 0 0
disk1@daily /mnt/disk1/.zfs/snapshot/daily zfs ro,relatime 0 0
my\040disk@snapraid-pending /mnt/my\040disk/.zfs/snapshot/snapraid-pending zfs ro 0 0
`
	write("mounts", host)
	link("self/ns/mnt", "mnt:[1]")
	link("1/ns/mnt", "mnt:[1]")
	write("1/mounts", host)
	link("469/ns/mnt", "mnt:[2]") // a sandboxed service: followed the host
	write("469/mounts", "disk1 /mnt/disk1 zfs rw 0 0\n")
	for _, pid := range []string{"2691736", "2691737"} { // a container
		link(pid+"/ns/mnt", "mnt:[3]")
		write(pid+"/comm", "diskindexer\n")
		write(pid+"/mounts", "overlay / overlay rw 0 0\ndisk5@snapraid-stable /mnt/disk5/.zfs/snapshot/snapraid-stable zfs ro 0 0\n")
	}

	r := &scriptRunner{out: map[string]string{"umount": ""}}
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), Runner: r})
	a.releaseSnapshots(context.Background())
	want := []string{
		"umount /mnt/disk5/.zfs/snapshot/snapraid-scan",
		"umount /mnt/my disk/.zfs/snapshot/snapraid-pending",
		"umount -N 2691736 /mnt/disk5/.zfs/snapshot/snapraid-stable",
	}
	if !slices.Equal(r.ran, want) {
		t.Errorf("ran %q", r.ran)
	}
}
