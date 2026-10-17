package agent

import (
	"context"
	"os"
	"strings"
	"time"
)

// Data disks that aren't ZFS pools. Homestack builds the array from
// single-disk ZFS pools, but SnapRAID works on any filesystem, and many
// arrays have ext4 or XFS data disks mounted from fstab. Those count as
// working while they're mounted, and stopping and starting the array
// unmounts and mounts them too. Replacing and adding disks stays ZFS-only
// (Homestack creates ZFS pools), and says how to do it by hand instead.

// dataFS says what filesystem a data disk's path holds: what's mounted
// there (mounted is it), else its fstab line; "" when neither says.
func (a *Agent) dataFS(ctx context.Context, path, fstab string) (fs string, mounted *Filesystem) {
	if f := a.fsInfo(ctx, path); f != nil {
		return f.FSType, f
	}
	if f := fstabEntry(fstab, path); f != nil {
		return f[2], nil
	}
	return "", nil
}

// fstabDevicePresent says whether the device fstab mounts at path is
// connected (a stopped array's disks are; a dead one's isn't).
func (a *Agent) fstabDevicePresent(ctx context.Context, path, fstab string) bool {
	f := fstabEntry(fstab, path)
	switch {
	case f == nil:
		return false
	case strings.HasPrefix(f[0], "/dev/"):
		_, err := os.Stat(a.sys(f[0]))
		return err == nil
	}
	_, err := a.cmd(ctx, 5*time.Second, "findfs", f[0]) // UUID=…, LABEL=…
	return err == nil
}

// plainDataDisks lists the data disks (snapraid.conf) that aren't ZFS.
func (a *Agent) plainDataDisks(ctx context.Context) []string {
	conf, err := readSnapConf(a.snapraidConf())
	if err != nil {
		return nil
	}
	fstab := a.readFstab()
	var out []string
	for _, p := range dataPaths(conf, "") {
		if fs, _ := a.dataFS(ctx, p, fstab); fs != "" && fs != "zfs" {
			out = append(out, p)
		}
	}
	return out
}
