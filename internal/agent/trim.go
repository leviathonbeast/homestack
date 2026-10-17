package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// TRIM tells the SSDs which blocks are free, so they stay fast. Debian's
// fstrim.timer does ordinary filesystems weekly; ZFS pools need zpool trim
// (or autotrim). Storage → Cache → TRIM runs both now, as a job with a log.

var (
	// "/mnt/cache: 41.2 GiB (44236840960 bytes) trimmed on /dev/nvme0n1p1"
	reFstrim = regexp.MustCompile(`^(.+): (\S+ \S+) \(\d+ bytes\) trimmed`)
	// fstrim --dry-run: "/mnt/cache: 0 B (dry run) trimmed on /dev/nvme0n1p1"
	reFstrimDry = regexp.MustCompile(`^(.+): 0 B \(dry run\) trimmed`)
	// A partition's kernel name: sdf1, nvme0n1p1.
	rePartName = regexp.MustCompile(`^(.+?)p?\d+$`)
)

var errTrimStopped = errors.New("you stopped it")

// trim trims every mounted filesystem on an SSD (fstrim skips the rest:
// hard disks, ZFS, mergerfs), one at a time so the log shows where it's got
// to (a first TRIM of a big SSD takes minutes), then each ZFS pool on SSDs.
// It returns a summary for the job list.
func (a *Agent) trim(ctx context.Context, pools []Pool, log func(string)) (string, error) {
	var done, failed []string
	mounts, listed := a.trimmable(ctx)
	switch {
	case !listed: // an fstrim that can't list them: all in one go
		log("Trimming the filesystems on SSDs (fstrim --all)")
		out, err := a.cmd(ctx, 3*time.Hour, "fstrim", "--all", "--verbose", "--quiet-unsupported")
		if ctx.Err() != nil {
			return "", errTrimStopped
		}
		for l := range strings.Lines(out) {
			if l = strings.TrimSpace(l); l != "" {
				log("  " + l)
			}
			if m := reFstrim.FindStringSubmatch(l); m != nil {
				done = append(done, fmt.Sprintf("%s (%s)", m[1], m[2]))
			}
		}
		if err != nil {
			failed = append(failed, "fstrim: "+why(out, err))
		}
	case len(mounts) == 0:
		log("No filesystem here is on an SSD that supports TRIM.")
	}
	for i, mp := range mounts {
		log(fmt.Sprintf("Trimming %s (%d of %d)", mp, i+1, len(mounts)))
		start := time.Now()
		out, err := a.cmd(ctx, 3*time.Hour, "fstrim", "--verbose", "--quiet-unsupported", mp)
		if ctx.Err() != nil {
			return "", errTrimStopped
		}
		m := reFstrim.FindStringSubmatch(strings.TrimSpace(out))
		switch {
		case err != nil:
			log("  Failed: " + why(out, err))
			failed = append(failed, mp+": "+why(out, err))
		case m != nil:
			log(fmt.Sprintf("  %s trimmed in %s.", m[2], humanDur(time.Since(start))))
			done = append(done, fmt.Sprintf("%s (%s)", mp, m[2]))
		default:
			log("  Its disk doesn't support TRIM.")
		}
	}

	// ZFS pools: not the ones on hard disks, which don't support TRIM.
	var ssd, hdd []string
	disks := a.disks.snapshot()
	for _, p := range pools {
		if a.onHardDisks(p, disks) {
			hdd = append(hdd, p.Name)
		} else {
			ssd = append(ssd, p.Name)
		}
	}
	if len(hdd) > 0 {
		log("Not trimming the ZFS pools on hard disks: " + strings.Join(hdd, ", ") + ".")
	}
	for _, p := range ssd {
		log("Trimming ZFS pool " + p + " (zpool trim)")
		start := time.Now()
		out, err := a.cmd(ctx, 24*time.Hour, "zpool", "trim", "-w", p)
		switch {
		case ctx.Err() != nil:
			// zpool trim -w only waits: the trim itself carries on unless cancelled.
			_, _ = a.cmd(context.Background(), 30*time.Second, "zpool", "trim", "-c", p)
			log("  Cancelled.")
			return "", errTrimStopped
		case err == nil:
			log("  Done in " + humanDur(time.Since(start)) + ".")
			done = append(done, "pool "+p)
		case strings.Contains(out, "not supported") || strings.Contains(out, "support trim"):
			// "trim operations are not supported by this device"
			log("  Its disks don't support TRIM.")
		default:
			log("  Failed: " + why(out, err))
			failed = append(failed, p+": "+why(out, err))
		}
	}
	sum := "Nothing here supports TRIM."
	if len(done) > 0 {
		sum = "Trimmed " + strings.Join(done, ", ") + "."
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("%s Failed: %s", sum, strings.Join(failed, "; "))
	}
	return sum, nil
}

// trimmable lists the filesystems fstrim --all would trim (its dry run).
// listed is false when fstrim couldn't say.
func (a *Agent) trimmable(ctx context.Context) (mounts []string, listed bool) {
	out, err := a.cmd(ctx, time.Minute, "fstrim", "--all", "--dry-run", "--verbose", "--quiet-unsupported")
	if err != nil {
		return nil, false
	}
	for l := range strings.Lines(out) {
		if m := reFstrimDry.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			mounts = append(mounts, m[1])
		} else if strings.TrimSpace(l) != "" {
			return nil, false // a format it doesn't know
		}
	}
	return mounts, true
}

// onHardDisks says whether a pool is only on hard disks. A disk it can't
// tell about might be an SSD, so the pool is tried.
func (a *Agent) onHardDisks(p Pool, disks []Disk) bool {
	if len(p.Devices) == 0 {
		return false
	}
	for _, dev := range p.Devices {
		name := a.devName(rePart.ReplaceAllString(dev, "")) // by-id: the whole disk
		if name == "" {
			name = strings.TrimPrefix(dev, "/dev/")
		}
		i := slices.IndexFunc(disks, func(d Disk) bool { return d.Name == name })
		if m := rePartName.FindStringSubmatch(name); i < 0 && m != nil { // /dev/sdf1
			i = slices.IndexFunc(disks, func(d Disk) bool { return d.Name == m[1] })
		}
		if i < 0 || !disks[i].Rotational {
			return false
		}
	}
	return true
}

// why is the last line a failed command printed, else its error.
func why(out string, err error) string {
	if l := lastLine(out); l != "" {
		return l
	}
	return err.Error()
}
