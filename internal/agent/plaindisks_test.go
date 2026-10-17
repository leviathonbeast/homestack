package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Data disks that aren't ZFS: working while mounted, still there (not
// mounted) when their device is connected, missing when it isn't; never
// replaced or added as ZFS.
func TestPlainDataDisks(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/fstab"), []byte(`UUID=root / ext4 defaults 0 1
UUID=aaa /mnt/disk2 ext4 defaults,nofail 0 2
UUID=bbb /mnt/disk3 xfs defaults,nofail 0 2
UUID=ccc /mnt/disk4 ext4 defaults,nofail 0 2
`), 0o644)
	conf := filepath.Join(root, "snapraid.conf")
	os.WriteFile(conf, []byte("parity /mnt/parity1/snapraid.parity\ndata d1 /mnt/disk1/\ndata d2 /mnt/disk2/\ndata d3 /mnt/disk3/\ndata d4 /mnt/disk4/\n"), 0o644)
	t.Setenv("HOMESTACK_SNAPRAID_CONF", conf)
	r := &scriptRunner{out: map[string]string{
		"findmnt -J -b -o TARGET,SOURCE,FSTYPE,SIZE,USED,AVAIL --mountpoint /mnt/disk1": `{"filesystems":[{"target":"/mnt/disk1","source":"disk1","fstype":"zfs","size":16000,"used":9000,"avail":7000}]}`,
		"findmnt -J -b -o TARGET,SOURCE,FSTYPE,SIZE,USED,AVAIL --mountpoint /mnt/disk2": `{"filesystems":[{"target":"/mnt/disk2","source":"/dev/sdc1","fstype":"ext4","size":12000,"used":5000,"avail":7000}]}`,
		"findfs UUID=bbb": "/dev/sdd1\n", // disk3: connected, not mounted (the array is stopped)
		// disk4 (UUID=ccc): not connected
	}}
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), Runner: r})
	ctx := context.Background()
	pools := []Pool{{Name: "disk1", Health: "ONLINE", Mountpoint: "/mnt/disk1", Mounted: true, Size: 16000, Alloc: 9000, Array: true}}
	ps := ParityStatus{Configured: true, Synced: true, Parity: []ParityDisk{{Mounted: true}}}
	got := map[string]ArraySlot{}
	for _, s := range a.slots(ctx, pools, []Disk{{Name: "sdc", Role: "disk2", Model: "WDC", Serial: "S2"}}, ps, false) {
		got[s.Pool] = s
	}

	if s := got["disk1"]; s.FSType != "zfs" || !s.healthy() || s.Method != "copy" {
		t.Errorf("ZFS disk1: %+v", s)
	}
	if s := got["disk2"]; s.FSType != "ext4" || !s.healthy() || s.PoolSize != 12000 || s.Used != 5000 || s.Serial != "S2" {
		t.Errorf("mounted ext4 disk2: %+v", s)
	}
	if s := got["disk2"]; s.Method != "" || !strings.Contains(s.Why, "snapraid -d d2 -l fix.log fix") {
		t.Errorf("disk2 must be replaced by hand: %q %q", s.Method, s.Why)
	}
	if s := got["disk3"]; s.FSType != "xfs" || s.State != "ONLINE" || s.Mounted {
		t.Errorf("xfs disk3, connected but not mounted: %+v", s)
	}
	if s := got["disk4"]; s.FSType != "ext4" || s.State != "MISSING" {
		t.Errorf("ext4 disk4, not connected: %+v", s)
	}
	if p := a.plainDataDisks(ctx); !slices.Equal(p, []string{"/mnt/disk2", "/mnt/disk3", "/mnt/disk4"}) {
		t.Errorf("plain data disks: %v", p)
	}
}
