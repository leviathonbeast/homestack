package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Copy and move in the file browser, like Unraid's file manager. A transfer
// runs as a storage job, so it never overlaps the mover or a parity job
// (files moved under a running parity check show up as missing in it).
//
// Guards:
//   - never between Shares (/mnt/user) and a single disk or the cache: those
//     are two views of the same files, so copying one onto "the other" can
//     overwrite a file with itself and lose it;
//   - never a folder into itself, or into the folder it's already in;
//   - SnapRAID's files, whole disks and whole shares stay where they are;
//   - enough free space, checked first;
//   - files are written under a temporary name and renamed into place, so a
//     stopped or failed transfer never leaves half a file under its name;
//   - a move deletes each original only after its copy is complete and on
//     disk;
//   - everything happens inside the folder holding both sides, through
//     os.Root, so a folder swapped for a symlink halfway can't lead the
//     root-run copy outside it.

// TransferRequest asks to copy or move items into a folder.
type TransferRequest struct {
	Op       string   `json:"op"` // copy | move
	Paths    []string `json:"paths"`
	Dest     string   `json:"dest"`
	Conflict string   `json:"conflict"` // skip (default) | replace: when the name is taken
}

// TransferPlan is what a transfer would do, for the confirmation.
type TransferPlan struct {
	Op        string   `json:"op"`
	Dest      string   `json:"dest"`
	Items     int      `json:"items"`
	Files     int      `json:"files"`
	Dirs      int      `json:"dirs"`
	Bytes     uint64   `json:"bytes"`
	Partial   bool     `json:"partial"`   // too many to count: at least these
	Conflicts []string `json:"conflicts"` // names already in the destination
	Free      uint64   `json:"free"`
	Instant   bool     `json:"instant"` // a move within one disk: renamed, nothing copied
	Warnings  []string `json:"warnings"`

	srcs   []string
	sizes  map[string]uint64 // per item
	counts map[string]int    // files per item
}

const (
	maxTransferItems = 1000
	planCountLimit   = 200000 // entries counted before saying "at least"
)

// view says which way of looking at the storage a root is.
func view(r *FileRoot) string {
	switch {
	case r.ID == "user":
		return "shares"
	case r.ID == "cache", r.ID != "disks" && strings.HasPrefix(r.ID, "disk"):
		return "disk"
	}
	return "other" // unassigned devices: separate filesystems
}

func (a *Agent) planTransfer(ctx context.Context, req TransferRequest) (*TransferPlan, error) {
	if req.Op != "copy" && req.Op != "move" {
		return nil, refuse("Unknown operation.")
	}
	if req.Conflict == "" {
		req.Conflict = "skip"
	}
	if req.Conflict != "skip" && req.Conflict != "replace" {
		return nil, refuse("Unknown choice for files that already exist.")
	}
	if len(req.Paths) == 0 {
		return nil, refuse("Pick something to %s.", req.Op)
	}
	if len(req.Paths) > maxTransferItems {
		return nil, refuse("Pick at most %d items at once (or their folder).", maxTransferItems)
	}
	dest, droot, err := a.resolve(ctx, req.Dest, true)
	if err != nil {
		return nil, refuse("%s", err.Error())
	}
	dfi, err := os.Stat(a.sys(dest))
	if err != nil || !dfi.IsDir() {
		return nil, refuse("%s isn't a folder.", dest)
	}
	destReal, _ := filepath.EvalSymlinks(a.sys(dest))
	verb := map[string]string{"copy": "copied", "move": "moved"}[req.Op]
	p := &TransferPlan{Op: req.Op, Dest: dest, Conflicts: []string{}, Warnings: []string{}, sizes: map[string]uint64{}, counts: map[string]int{}}
	names := map[string]string{}
	sameFS := true
	for _, raw := range req.Paths {
		src, sroot, err := a.resolve(ctx, raw, true)
		if err != nil {
			return nil, refuse("%s", err.Error())
		}
		base := filepath.Base(src)
		if other, dup := names[base]; dup {
			if other == src {
				continue
			}
			return nil, refuse("Two of the items are called %s; they'd land on top of each other in %s.", base, dest)
		}
		names[base] = src
		switch {
		case src == sroot.Path:
			return nil, refuse("%s is a whole disk or view; pick the folders inside it.", src)
		case protectedName(src):
			return nil, refuse("%s is one of SnapRAID's own files; it stays where it is.", src)
		case req.Op == "move" && sroot.ID == "user" && filepath.Dir(src) == sroot.Path:
			return nil, refuse("%s is a whole share. Shares aren't moved from here: move the folders inside it instead.", src)
		}
		if vs, vd := view(sroot), view(droot); vs != vd && vs != "other" && vd != "other" {
			return nil, refuse("Copying or moving between Shares and a single disk (or the cache) isn't allowed: they're two views of the same files, so a file could be written over itself and lost. Use Shares on both sides, or disks on both sides.")
		}
		if filepath.Dir(src) == dest {
			return nil, refuse("%s is already in %s. Open the folder it should go to, then paste.", base, dest)
		}
		srcReal, _ := filepath.EvalSymlinks(a.sys(src))
		if dest == src || strings.HasPrefix(dest, src+"/") || destReal == srcReal || strings.HasPrefix(destReal, srcReal+"/") {
			return nil, refuse("%s can't be %s into itself.", base, verb)
		}
		if _, err := os.Lstat(a.sys(filepath.Join(dest, base))); err == nil {
			p.Conflicts = append(p.Conflicts, base)
		}
		if !sameDevice(a.sys(src), a.sys(dest)) {
			sameFS = false
		}
		p.srcs = append(p.srcs, src)
	}
	p.Items = len(p.srcs)

	// Count what's in them (until it takes too long to say).
	seen := 0
	for _, src := range p.srcs {
		var size uint64
		files := p.Files
		_ = filepath.WalkDir(a.sys(src), func(path string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil {
				return nil
			}
			if seen++; seen > planCountLimit {
				p.Partial = true
				return fs.SkipAll
			}
			switch {
			case d.IsDir():
				p.Dirs++
			case d.Type().IsRegular():
				p.Files++
				if fi, err := d.Info(); err == nil {
					size += uint64(fi.Size())
				}
			default:
				p.Files++ // links and the like count, at no size
			}
			return nil
		})
		p.sizes[src] = size
		p.counts[src] = p.Files - files
		p.Bytes += size
	}
	var st syscall.Statfs_t
	if syscall.Statfs(a.sys(dest), &st) == nil {
		p.Free = st.Bavail * uint64(st.Bsize)
	}
	p.Instant = req.Op == "move" && sameFS
	if !p.Instant {
		if p.Bytes > p.Free {
			return nil, refuse("Not enough space: this needs %s and %s has %s free.", sizeText(p.Bytes), dest, sizeText(p.Free))
		}
		if p.Bytes > p.Free/10*9 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("This nearly fills %s (%s of %s free).", dest, sizeText(p.Bytes), sizeText(p.Free)))
		}
	}
	if p.Partial {
		p.Warnings = append(p.Warnings, fmt.Sprintf("There are more than %d items, so the size is a lower limit.", planCountLimit))
	}
	if droot.ID == "user" && !p.Instant && p.Bytes > 0 {
		p.Warnings = append(p.Warnings, "Shares spread files over the disks, so a big file still needs room on one disk.")
	}
	return p, nil
}

func sameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Lstat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}

// planFiles answers POST /v1/files/plan: what a copy or move would do.
func (a *Agent) planFiles(w http.ResponseWriter, r *http.Request) {
	var req TransferRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	p, err := a.planTransfer(ctx, req)
	if err != nil {
		if rf, ok := errors.AsType[refusal](err); ok {
			writeErr(w, 409, rf.msg)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if h := a.jobs.lockHolder(); h != "" {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s is running; this can start once it has finished.", h))
	}
	writeJSON(w, 200, p)
}

// ---------- Doing it ----------

type xfer struct {
	a       *Agent
	ctx     context.Context
	root    *os.Root // /mnt
	move    bool
	replace bool
	log     func(string)

	total, done      uint64 // for progress: skipped files count as done
	copied           uint64 // what was actually copied or moved
	files, skipped   int
	failed           int
	started, lastTik time.Time
}

// commonDir is the deepest folder that holds every one of paths.
func commonDir(paths ...string) string {
	c := filepath.Clean(paths[0])
	for _, p := range paths[1:] {
		p = filepath.Clean(p)
		for c != "/" && p != c && !strings.HasPrefix(p, c+"/") {
			c = filepath.Dir(c)
		}
	}
	return c
}

func (a *Agent) runTransfer(ctx context.Context, p *TransferPlan, replace bool, log func(string)) error {
	base := commonDir(append([]string{p.Dest}, p.srcs...)...)
	root, err := os.OpenRoot(a.sys(base))
	if err != nil {
		return err
	}
	defer root.Close()
	x := &xfer{a: a, ctx: ctx, root: root, move: p.Op == "move", replace: replace, log: log, total: p.Bytes, started: time.Now()}
	verb := map[bool]string{false: "Copying", true: "Moving"}[x.move]
	rel := func(path string) string {
		if r, err := filepath.Rel(base, path); err == nil {
			return r
		}
		return path // refused by the root: it's absolute
	}
	for _, src := range p.srcs {
		if ctx.Err() != nil {
			break
		}
		log(fmt.Sprintf("%s %s to %s", verb, src, p.Dest))
		dst := filepath.Join(p.Dest, filepath.Base(src))
		if x.move {
			// Within one disk, a rename moves the lot at once.
			if _, err := root.Lstat(rel(dst)); errors.Is(err, fs.ErrNotExist) {
				err := root.Rename(rel(src), rel(dst))
				if err == nil {
					x.done += p.sizes[src]
					x.copied += p.sizes[src]
					x.files += max(p.counts[src], 1)
					x.tick(true, filepath.Base(src))
					continue
				}
				if !errors.Is(err, syscall.EXDEV) {
					x.fail(src, err)
					continue
				}
			}
		}
		x.tree(rel(src), rel(dst), src)
	}
	x.tick(true, "")
	what := map[bool]string{false: "copied", true: "moved"}[x.move]
	summary := fmt.Sprintf("%d file%s %s (%s)", x.files, plural(x.files), what, sizeText(x.copied))
	if x.skipped > 0 {
		summary += fmt.Sprintf(", %d skipped because %s already there", x.skipped, map[bool]string{true: "it was", false: "they were"}[x.skipped == 1])
	}
	if x.failed > 0 {
		summary += fmt.Sprintf(", %d failed (see the log)", x.failed)
	}
	summary += "."
	log(summary)
	if ctx.Err() != nil {
		return fmt.Errorf("stopped: %s Nothing was left half-written", strings.TrimSuffix(summary, "."))
	}
	a.jobs.setProgress(func(j *Job) { j.Detail = summary })
	if x.failed > 0 {
		return errors.New(summary)
	}
	return nil
}

func (x *xfer) fail(shown string, err error) {
	x.failed++
	if x.failed <= 200 {
		x.log(fmt.Sprintf("couldn't %s %s: %v", map[bool]string{false: "copy", true: "move"}[x.move], shown, err))
	} else if x.failed == 201 {
		x.log("(more failures not listed)")
	}
}

// tree copies (or moves) src to dst, both relative to /mnt. For a move it
// says whether the source is entirely gone, so its folder can go too.
func (x *xfer) tree(src, dst, shown string) bool {
	if x.ctx.Err() != nil {
		return false
	}
	fi, err := x.root.Lstat(src)
	if err != nil {
		x.fail(shown, err)
		return false
	}
	if protectedName(src) {
		x.log("left " + shown + " alone (a SnapRAID file)")
		return false
	}
	switch {
	case fi.IsDir():
		return x.dir(src, dst, shown, fi)
	case fi.Mode().IsRegular():
		return x.file(src, dst, shown, fi)
	case fi.Mode()&fs.ModeSymlink != 0:
		return x.link(src, dst, shown, fi)
	}
	x.log("skipped " + shown + " (a device, pipe or socket)")
	return false
}

func (x *xfer) dir(src, dst, shown string, fi os.FileInfo) bool {
	created := false
	dfi, err := x.root.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := x.root.Mkdir(dst, 0o700); err != nil {
			x.fail(shown, err)
			return false
		}
		created = true
	case err != nil:
		x.fail(shown, err)
		return false
	case !dfi.IsDir():
		x.fail(shown, errors.New("a file with that name is already there"))
		return false
	case os.SameFile(fi, dfi):
		x.fail(shown, errors.New("it's the same folder"))
		return false
	}
	f, err := x.root.Open(src)
	if err != nil {
		x.fail(shown, err)
		return false
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	gone := err == nil
	if err != nil {
		x.fail(shown, err)
	}
	for _, e := range entries {
		if x.ctx.Err() != nil {
			gone = false
			break
		}
		if !x.tree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), filepath.Join(shown, e.Name())) {
			gone = false
		}
	}
	if created {
		x.meta(dst, fi) // last, so writing into it doesn't change its time
	}
	if x.move && gone {
		if err := x.root.Remove(src); err != nil {
			x.fail(shown, fmt.Errorf("everything was moved, but the empty folder couldn't be removed: %v", err))
			return false
		}
	}
	return gone
}

func (x *xfer) file(src, dst, shown string, fi os.FileInfo) bool {
	size := uint64(fi.Size())
	if dfi, err := x.root.Lstat(dst); err == nil {
		switch {
		case os.SameFile(fi, dfi):
			x.fail(shown, errors.New("the original and the destination are the same file"))
			return false
		case dfi.IsDir():
			x.fail(shown, errors.New("a folder with that name is already there"))
			return false
		case !x.replace:
			x.skipped++
			x.done += size
			return false // for a move, the original stays
		}
	}
	if x.move { // same disk: a rename, which also replaces atomically
		err := x.root.Rename(src, dst)
		if err == nil {
			x.files++
			x.done += size
			x.copied += size
			x.tick(false, shown)
			return true
		}
		if !errors.Is(err, syscall.EXDEV) {
			x.fail(shown, err)
			return false
		}
	}
	in, err := x.root.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		x.fail(shown, err)
		return false
	}
	defer in.Close()
	if now, err := in.Stat(); err != nil || !os.SameFile(fi, now) {
		x.fail(shown, errors.New("it changed while being copied"))
		return false
	}
	tmp := filepath.Join(filepath.Dir(dst), ".hs-part-"+randHex(6))
	out, err := x.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		x.fail(shown, err)
		return false
	}
	discard := func(err error) bool {
		out.Close()
		_ = x.root.Remove(tmp)
		if x.ctx.Err() == nil {
			x.fail(shown, err)
		}
		return false
	}
	var n uint64
	for {
		if err := x.ctx.Err(); err != nil {
			return discard(err)
		}
		// In chunks, so Stop is quick and progress moves; io.CopyN lets the
		// kernel copy (copy_file_range) where the filesystems allow.
		c, err := io.CopyN(out, in, 64<<20)
		n += uint64(c)
		x.done += uint64(c)
		x.tick(false, shown)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return discard(err)
		}
	}
	if n != size {
		return discard(fmt.Errorf("it changed size while being copied (%d of %d bytes)", n, size))
	}
	if x.move { // on disk before the original goes
		if err := out.Sync(); err != nil {
			return discard(err)
		}
	}
	if err := out.Close(); err != nil {
		_ = x.root.Remove(tmp)
		x.fail(shown, err)
		return false
	}
	x.meta(tmp, fi)
	if err := x.root.Rename(tmp, dst); err != nil {
		_ = x.root.Remove(tmp)
		x.fail(shown, err)
		return false
	}
	x.files++
	x.copied += size
	if x.move {
		if err := x.root.Remove(src); err != nil {
			x.fail(shown, fmt.Errorf("copied, but the original couldn't be removed: %v", err))
			return false
		}
	}
	return true
}

func (x *xfer) link(src, dst, shown string, fi os.FileInfo) bool {
	target, err := x.root.Readlink(src)
	if err != nil {
		x.fail(shown, err)
		return false
	}
	if dfi, err := x.root.Lstat(dst); err == nil {
		if !x.replace {
			x.skipped++
			return false
		}
		if dfi.IsDir() {
			x.fail(shown, errors.New("a folder with that name is already there"))
			return false
		}
		if err := x.root.Remove(dst); err != nil {
			x.fail(shown, err)
			return false
		}
	}
	// The link is copied as a link: what it points to isn't read or followed.
	if err := x.root.Symlink(target, dst); err != nil {
		x.fail(shown, err)
		return false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = x.root.Lchown(dst, int(st.Uid), int(st.Gid))
	}
	x.files++
	if x.move {
		if err := x.root.Remove(src); err != nil {
			x.fail(shown, err)
			return false
		}
	}
	return true
}

// meta gives a copy its original's owner, permissions and modified time.
func (x *xfer) meta(path string, fi os.FileInfo) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = x.root.Lchown(path, int(st.Uid), int(st.Gid))
	}
	_ = x.root.Chmod(path, fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	_ = x.root.Chtimes(path, fi.ModTime(), fi.ModTime())
}

// tick updates the job's progress, at most once a second.
func (x *xfer) tick(force bool, current string) {
	now := time.Now()
	if !force && now.Sub(x.lastTik) < time.Second {
		return
	}
	x.lastTik = now
	done, total := x.done, max(x.total, x.done)
	el := now.Sub(x.started).Seconds()
	x.a.jobs.setProgress(func(j *Job) {
		pct := 100.0
		if total > 0 {
			pct = min(99.9, float64(done)/float64(total)*100)
		}
		j.Progress, j.Done, j.Total = &pct, &done, &total
		if el >= 2 && done > 0 {
			rate := float64(done) / el
			j.Speed = fmt.Sprintf("%.0f MB/s", rate/1e6)
			secs := int64(float64(total-done) / rate)
			j.ETASecs = &secs
			j.ETA = fmt.Sprintf("%d:%02d", secs/3600, secs%3600/60)
		}
		if current != "" {
			j.Step = current
		}
	})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sizeText is human() for any size, small files included: "512 KB".
func sizeText(b uint64) string {
	switch {
	case b < 1000:
		return fmt.Sprintf("%d bytes", b)
	case b < 1e6:
		return fmt.Sprintf("%.0f KB", float64(b)/1e3)
	}
	return human(b)
}
