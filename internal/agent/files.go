package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// A basic file browser over the storage: list, download, edit text files,
// new file or folder, rename, delete, and Unraid's "New Permissions".
// Everything stays inside the roots below, symlinks included.

type FileRoot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Path  string `json:"path"`
}

func (a *Agent) fileRoots(ctx context.Context) []FileRoot {
	l := a.layout()
	cands := []FileRoot{{"user", "Shares", l.Shares}, {"cache", "Cache", l.Cache}}
	for _, p := range a.dataDiskPaths(l) {
		n := l.DiskNum(p)
		cands = append(cands, FileRoot{fmt.Sprintf("disk%d", n), filepath.Base(p), p})
	}
	cands = append(cands, FileRoot{"disks", "Unassigned", l.Unassigned})
	var out []FileRoot
	for _, r := range cands {
		if fi, err := os.Stat(a.sys(r.Path)); err == nil && fi.IsDir() {
			if r.ID == "disks" || a.isMounted(ctx, r.Path) {
				out = append(out, r)
			}
		}
	}
	return out
}

var errOutside = errors.New("That path is outside the storage.")

// resolve checks a path from the browser: absolute, inside a root, and (for
// existing paths) still inside once symlinks are followed.
func (a *Agent) resolve(ctx context.Context, p string, mustExist bool) (string, *FileRoot, error) {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", nil, errOutside
	}
	p = filepath.Clean(p)
	roots := a.fileRoots(ctx)
	var root *FileRoot
	for i := range roots {
		if p == roots[i].Path || strings.HasPrefix(p, roots[i].Path+"/") {
			root = &roots[i]
		}
	}
	if root == nil {
		return "", nil, errOutside
	}
	real := a.sys(p)
	if r, err := filepath.EvalSymlinks(real); err == nil {
		inside := false
		for _, rt := range roots {
			rr, _ := filepath.EvalSymlinks(a.sys(rt.Path))
			if r == rr || strings.HasPrefix(r, rr+"/") {
				inside = true
			}
		}
		if !inside {
			return "", nil, errOutside
		}
	} else if mustExist {
		return "", nil, fmt.Errorf("%s doesn't exist.", p)
	}
	return p, root, nil
}

type FileEntry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Link    string    `json:"link,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Mode    string    `json:"mode"`
	Owner   string    `json:"owner"`
	Group   string    `json:"group"`
	Disks   []string  `json:"disks,omitempty"` // for /mnt/user: which disk(s) hold it
}

var nameCache = map[string]string{}

func lookupName(id uint32, group bool) string {
	key := fmt.Sprintf("%v:%d", group, id)
	if n, ok := nameCache[key]; ok {
		return n
	}
	n := strconv.Itoa(int(id))
	if group {
		if g, err := user.LookupGroupId(n); err == nil {
			n = g.Name
		}
	} else if u, err := user.LookupId(n); err == nil {
		n = u.Username
	}
	if id == 99 && !group && n == "99" {
		n = "nobody (99)"
	}
	nameCache[key] = n
	return n
}

// dataDiskPaths are the data disks' mount points that exist, in order.
func (a *Agent) dataDiskPaths(l Layout) []string {
	found, _ := filepath.Glob(a.sys(l.DiskPrefix) + "[1-9]*")
	var out []string
	for _, f := range found {
		p := filepath.Join("/", strings.TrimPrefix(f, a.opt.SysRoot))
		if l.DiskNum(p) > 0 {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(x, y string) int { return l.DiskNum(x) - l.DiskNum(y) })
	return out
}

// diskOf reads mergerfs's "which branches hold this" attribute, as disk
// names ("cache", "disk3").
func diskOf(l Layout, path string) []string {
	buf := make([]byte, 4096)
	n, err := syscall.Getxattr(path, "user.mergerfs.allpaths", buf)
	if err != nil || n <= 0 {
		return nil
	}
	var out []string
	for p := range strings.SplitSeq(string(buf[:n]), "\x00") {
		if p == "" {
			continue
		}
		switch w := l.where(p); w {
		case "":
		case "the cache":
			out = append(out, "cache")
		default:
			out = append(out, w)
		}
	}
	return out
}

const maxList = 5000

func (a *Agent) listFiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	roots := a.fileRoots(ctx)
	p := r.URL.Query().Get("path")
	if p == "" {
		writeJSON(w, 200, map[string]any{"roots": roots, "owner": a.layout().ownerName()})
		return
	}
	p, root, err := a.resolve(ctx, p, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	real := a.sys(p)
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		writeErr(w, 400, "Not a folder.")
		return
	}
	des, err := os.ReadDir(real)
	if err != nil {
		writeErr(w, 400, "Can't read that folder: "+err.Error())
		return
	}
	withDisks, lay := root.ID == "user", a.layout()
	entries := make([]FileEntry, 0, len(des))
	truncated := false
	for i, de := range des {
		if i >= maxList {
			truncated = true
			break
		}
		full := filepath.Join(real, de.Name())
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		e := FileEntry{Name: de.Name(), Size: info.Size(), ModTime: info.ModTime(), Mode: info.Mode().String()}
		if info.Mode()&os.ModeSymlink != 0 {
			e.Link, _ = os.Readlink(full)
			if t, err := os.Stat(full); err == nil {
				e.Dir = t.IsDir()
				e.Size = t.Size()
			}
		} else {
			e.Dir = info.IsDir()
		}
		if e.Dir {
			e.Size = 0
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			e.Owner, e.Group = lookupName(st.Uid, false), lookupName(st.Gid, true)
		}
		if withDisks {
			e.Disks = diskOf(lay, full)
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	var free, size uint64
	var sfs syscall.Statfs_t
	if syscall.Statfs(real, &sfs) == nil {
		free, size = sfs.Bavail*uint64(sfs.Bsize), sfs.Blocks*uint64(sfs.Bsize)
	}
	writeJSON(w, 200, map[string]any{"roots": roots, "owner": lay.ownerName(), "root": root, "path": p, "entries": entries, "truncated": truncated, "free": free, "size": size})
}

func (a *Agent) downloadFile(w http.ResponseWriter, r *http.Request) {
	p, _, err := a.resolve(r.Context(), r.URL.Query().Get("path"), true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	f, err := os.Open(a.sys(p))
	if err != nil {
		writeErr(w, 400, "Can't open that file.")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeErr(w, 400, "Only files can be downloaded.")
		return
	}
	name := filepath.Base(p)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, f)
}

// protectedName: files the browser never changes.
func protectedName(p string) bool {
	b := filepath.Base(p)
	return strings.HasPrefix(b, ".snapraid.content") || strings.HasPrefix(b, "snapraid.content") || strings.HasSuffix(b, ".parity")
}

func validName(n string) bool {
	return n != "" && n != "." && n != ".." && len(n) <= 255 && !strings.ContainsAny(n, "/\x00")
}

func (a *Agent) fileOp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op      string `json:"op"` // mkdir, mkfile, rename, delete
		Path    string `json:"path"`
		Name    string `json:"name"`
		Confirm string `json:"confirm"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	ctx := r.Context()
	fail := func(msg string) { writeErr(w, 400, msg) }
	p, root, err := a.resolve(ctx, req.Path, true)
	if err != nil {
		fail(err.Error())
		return
	}
	isRoot := p == root.Path
	uid, gid := a.layout().OwnerIDs() // new files and folders belong to the shares' owner
	switch req.Op {
	case "mkdir":
		if !validName(req.Name) {
			fail("That isn't a valid folder name.")
			return
		}
		target := filepath.Join(p, req.Name)
		if err := os.Mkdir(a.sys(target), 0o777); err != nil {
			fail("Couldn't create it: " + err.Error())
			return
		}
		_ = os.Chmod(a.sys(target), 0o777)
		_ = os.Chown(a.sys(target), uid, gid)
	case "mkfile":
		if !validName(req.Name) || protectedName(req.Name) {
			fail("That isn't a valid file name.")
			return
		}
		target := filepath.Join(p, req.Name)
		f, err := os.OpenFile(a.sys(target), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if errors.Is(err, fs.ErrExist) {
			fail("Something with that name already exists here.")
			return
		} else if err != nil {
			fail("Couldn't create it: " + err.Error())
			return
		}
		f.Close()
		_ = os.Chmod(a.sys(target), 0o666)
		_ = os.Chown(a.sys(target), uid, gid)
	case "rename":
		if isRoot || !validName(req.Name) || protectedName(p) {
			fail("That can't be renamed.")
			return
		}
		target := filepath.Join(filepath.Dir(p), req.Name)
		if _, err := os.Lstat(a.sys(target)); err == nil {
			fail("Something with that name already exists here.")
			return
		}
		if err := os.Rename(a.sys(p), a.sys(target)); err != nil {
			fail("Couldn't rename it: " + err.Error())
			return
		}
	case "delete":
		if isRoot || protectedName(p) {
			fail("That can't be deleted here.")
			return
		}
		fi, err := os.Lstat(a.sys(p))
		if err != nil {
			fail("It's already gone.")
			return
		}
		if fi.IsDir() {
			// A folder: type its name to confirm. A top-level share folder on
			// /mnt/user must be empty first (as on Unraid).
			if req.Confirm != filepath.Base(p) {
				fail("Type the folder's name to confirm.")
				return
			}
			if root.ID == "user" && filepath.Dir(p) == root.Path {
				if des, _ := os.ReadDir(a.sys(p)); len(des) > 0 {
					fail("A share's top folder can only be deleted once it's empty.")
					return
				}
			}
			err = os.RemoveAll(a.sys(p))
		} else {
			err = os.Remove(a.sys(p))
		}
		if err != nil {
			fail("Couldn't delete it: " + err.Error())
			return
		}
	default:
		fail("unknown operation")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- Editing text files ----------

// maxEditBytes: bigger files are downloaded rather than edited in the browser.
const maxEditBytes = 2 << 20

// fileStamp identifies the version of a file the editor opened, so a save
// doesn't overwrite changes made elsewhere in the meantime.
func fileStamp(fi os.FileInfo) string {
	return fmt.Sprintf("%s/%d", fi.ModTime().UTC().Format(time.RFC3339Nano), fi.Size())
}

// editable opens a path for the editor: a regular text file of at most
// maxEditBytes, outside the protected names.
func (a *Agent) editable(ctx context.Context, path string) (string, os.FileInfo, error) {
	p, _, err := a.resolve(ctx, path, true)
	if err != nil {
		return "", nil, err
	}
	if protectedName(p) {
		return "", nil, errors.New("SnapRAID's own files can't be edited here.")
	}
	fi, err := os.Stat(a.sys(p))
	if err != nil {
		return "", nil, fmt.Errorf("%s doesn't exist.", p)
	}
	if !fi.Mode().IsRegular() {
		return "", nil, errors.New("Only files can be edited.")
	}
	if fi.Size() > maxEditBytes {
		return "", nil, fmt.Errorf("That file is %.1f MB; files up to 2 MB can be edited here. Download it instead.", float64(fi.Size())/(1<<20))
	}
	return p, fi, nil
}

// isText: valid UTF-8 without NUL bytes (which binary files almost always have).
func isText(b []byte) bool {
	return utf8.Valid(b) && !bytes.ContainsRune(b, 0)
}

// readFileContent returns a text file for the editor. CRLF files are sent
// with plain newlines and written back with CRLF, since the editor works
// with \n only.
func (a *Agent) readFileContent(w http.ResponseWriter, r *http.Request) {
	p, fi, err := a.editable(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	b, err := os.ReadFile(a.sys(p))
	if err != nil {
		writeErr(w, 400, "Can't read that file: "+err.Error())
		return
	}
	if !isText(b) {
		writeErr(w, 415, "That isn't a text file, so it can't be edited here. Download it instead.")
		return
	}
	text, eol := string(b), "lf"
	if n := strings.Count(text, "\r\n"); n > 0 && n == strings.Count(text, "\n") {
		text, eol = strings.ReplaceAll(text, "\r\n", "\n"), "crlf"
	}
	writeJSON(w, 200, map[string]any{"path": p, "content": text, "eol": eol, "size": fi.Size(), "stamp": fileStamp(fi), "mtime": fi.ModTime()})
}

// writeFileContent saves the editor's text over the file, in place, so its
// owner, permissions and hard links stay as they were.
func (a *Agent) writeFileContent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		EOL     string `json:"eol"`   // lf | crlf
		Stamp   string `json:"stamp"` // from readFileContent
		Force   bool   `json:"force"` // overwrite even if it changed since
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8*maxEditBytes)).Decode(&req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	p, fi, err := a.editable(r.Context(), req.Path)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !req.Force && req.Stamp != fileStamp(fi) {
		writeJSON(w, 409, map[string]any{"error": "The file changed on disk since you opened it.", "conflict": true})
		return
	}
	text := req.Content
	if req.EOL == "crlf" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if len(text) > maxEditBytes {
		writeErr(w, 400, "That's more than 2 MB, too big to save from here.")
		return
	}
	if !utf8.ValidString(text) {
		writeErr(w, 400, "The text isn't valid UTF-8.")
		return
	}
	real := a.sys(p)
	old, err := os.ReadFile(real)
	if err != nil {
		writeErr(w, 400, "Can't read that file: "+err.Error())
		return
	}
	f, err := os.OpenFile(real, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		writeErr(w, 400, "Can't write that file: "+err.Error())
		return
	}
	_, werr := f.WriteString(text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.WriteFile(real, old, 0) // put the old text back
		writeErr(w, 500, "Couldn't save it (the old text was put back): "+werr.Error())
		return
	}
	if fi, err = os.Stat(real); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "size": fi.Size(), "stamp": fileStamp(fi), "mtime": fi.ModTime()})
}

// fixPermissions is Unraid's "New Permissions": the shares' owner (Layout,
// nobody:users by default), folders 777, files 666. Runs as a job.
func (a *Agent) fixPermissions(ctx context.Context, path string, log func(string)) error {
	l := a.layout()
	uid, gid := l.OwnerIDs()
	n := 0
	err := filepath.WalkDir(a.sys(path), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			log("skipped " + p + ": " + err.Error())
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Type()&fs.ModeSymlink != 0 || protectedName(p) {
			return nil
		}
		_ = os.Lchown(p, uid, gid)
		if d.IsDir() {
			_ = os.Chmod(p, 0o777)
		} else if d.Type().IsRegular() {
			_ = os.Chmod(p, 0o666)
		}
		n++
		if n%20000 == 0 {
			log(fmt.Sprintf("%d items done…", n))
		}
		return nil
	})
	log(fmt.Sprintf("%d items set to %s, folders 777, files 666.", n, l.ownerName()))
	return err
}
