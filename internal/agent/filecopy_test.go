package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mountedRunner says every path is mounted (findmnt), so the share and disk
// roots show up.
type mountedRunner struct{}

func (mountedRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

func transferAgent(t *testing.T) (*Agent, string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"mnt/user/media/films", "mnt/user/backup", "mnt/disk1/media", "mnt/disk2", "mnt/disks/usb", "etc"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	w := func(p, s string) { os.WriteFile(filepath.Join(root, p), []byte(s), 0o640) }
	w("mnt/user/media/films/a.mkv", "film a")
	w("mnt/user/media/films/b.mkv", "film bb")
	w("mnt/user/media/notes.txt", "notes")
	w("mnt/disk1/.snapraid.content", "x")
	w("etc/passwd", "root:x:0:0")
	os.Symlink(filepath.Join(root, "etc/passwd"), filepath.Join(root, "mnt/user/media/films/escape"))
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(root, "mnt/user/media/films/a.mkv"), old, old)
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), LogDir: t.TempDir(), Runner: mountedRunner{}, LockFile: filepath.Join(t.TempDir(), "lock")})
	return a, root
}

func TestTransferGuards(t *testing.T) {
	a, root := transferAgent(t)
	os.MkdirAll(filepath.Join(root, "mnt/user/backup/films"), 0o755)
	ctx := context.Background()
	bad := map[string]TransferRequest{
		"shares to a disk":     {Op: "copy", Paths: []string{"/mnt/user/media/notes.txt"}, Dest: "/mnt/disk1/media"},
		"a disk to shares":     {Op: "move", Paths: []string{"/mnt/disk1/media"}, Dest: "/mnt/user/backup"},
		"into itself":          {Op: "copy", Paths: []string{"/mnt/user/media"}, Dest: "/mnt/user/media/films"},
		"onto itself":          {Op: "copy", Paths: []string{"/mnt/user/media/films"}, Dest: "/mnt/user/media/films"},
		"same folder":          {Op: "copy", Paths: []string{"/mnt/user/media/notes.txt"}, Dest: "/mnt/user/media"},
		"snapraid file":        {Op: "copy", Paths: []string{"/mnt/disk1/.snapraid.content"}, Dest: "/mnt/disk2"},
		"a whole share (move)": {Op: "move", Paths: []string{"/mnt/user/media"}, Dest: "/mnt/user/backup"},
		"a whole disk":         {Op: "copy", Paths: []string{"/mnt/disk1"}, Dest: "/mnt/disk2"},
		"outside the storage":  {Op: "copy", Paths: []string{"/etc/passwd"}, Dest: "/mnt/disk2"},
		"to outside":           {Op: "copy", Paths: []string{"/mnt/disk1/media"}, Dest: "/etc"},
		"a file as the dest":   {Op: "copy", Paths: []string{"/mnt/user/backup"}, Dest: "/mnt/user/media/notes.txt"},
		"missing":              {Op: "copy", Paths: []string{"/mnt/user/media/nope"}, Dest: "/mnt/user/backup"},
		"same names":           {Op: "copy", Paths: []string{"/mnt/user/media/films", "/mnt/user/backup/films"}, Dest: "/mnt/disks/usb"},
		"unknown op":           {Op: "link", Paths: []string{"/mnt/user/media/notes.txt"}, Dest: "/mnt/user/backup"},
		"nothing":              {Op: "copy", Dest: "/mnt/user/backup"},
	}
	for name, req := range bad {
		if _, err := a.planTransfer(ctx, req); err == nil {
			t.Errorf("%s: allowed", name)
		} else if name == "same names" && !strings.Contains(err.Error(), "same") && !strings.Contains(err.Error(), "called") {
			t.Errorf("same names refused for another reason: %v", err)
		}
	}
	// Unassigned devices are separate: to and from them is fine.
	if _, err := a.planTransfer(ctx, TransferRequest{Op: "copy", Paths: []string{"/mnt/user/media/notes.txt"}, Dest: "/mnt/disks/usb"}); err != nil {
		t.Errorf("shares to an unassigned disk: %v", err)
	}
	p, err := a.planTransfer(ctx, TransferRequest{Op: "move", Paths: []string{"/mnt/user/media/films", "/mnt/user/media/notes.txt"}, Dest: "/mnt/user/backup"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Items != 2 || p.Files != 4 || p.Dirs != 1 || p.Bytes != uint64(len("film a")+len("film bb")+len("notes")) || !p.Instant || len(p.Conflicts) != 1 || p.Conflicts[0] != "films" {
		t.Fatalf("plan %+v", p)
	}
}

func TestTransferCopyAndMove(t *testing.T) {
	a, root := transferAgent(t)
	ctx := context.Background()
	var log []string
	logf := func(s string) { log = append(log, s) }
	run := func(req TransferRequest) {
		t.Helper()
		p, err := a.planTransfer(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.runTransfer(ctx, p, req.Conflict == "replace", logf); err != nil {
			t.Fatalf("%v\n%s", err, strings.Join(log, "\n"))
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(root, p)); return string(b) }

	// Copy a folder: contents, times and modes kept; links copied as links.
	run(TransferRequest{Op: "copy", Paths: []string{"/mnt/user/media/films"}, Dest: "/mnt/user/backup"})
	if read("mnt/user/backup/films/a.mkv") != "film a" || read("mnt/user/media/films/a.mkv") != "film a" {
		t.Fatal("copy")
	}
	fi, _ := os.Stat(filepath.Join(root, "mnt/user/backup/films/a.mkv"))
	if fi.ModTime().Year() != 2020 || fi.Mode().Perm() != 0o640 {
		t.Fatalf("metadata: %v %v", fi.ModTime(), fi.Mode())
	}
	if target, err := os.Readlink(filepath.Join(root, "mnt/user/backup/films/escape")); err != nil || target != filepath.Join(root, "etc/passwd") {
		t.Fatalf("link: %q %v (must stay a link, not become a copy of what it points to)", target, err)
	}

	// Again: existing files are skipped unless replacing.
	os.WriteFile(filepath.Join(root, "mnt/user/media/films/a.mkv"), []byte("film a, new cut"), 0o640)
	run(TransferRequest{Op: "copy", Paths: []string{"/mnt/user/media/films"}, Dest: "/mnt/user/backup"})
	if read("mnt/user/backup/films/a.mkv") != "film a" {
		t.Fatal("skip overwrote")
	}
	run(TransferRequest{Op: "copy", Paths: []string{"/mnt/user/media/films"}, Dest: "/mnt/user/backup", Conflict: "replace"})
	if read("mnt/user/backup/films/a.mkv") != "film a, new cut" {
		t.Fatal("replace didn't")
	}

	// Move into a folder that already has one of the files: the rest move,
	// the clash stays where it was (skip), and its folder with it.
	os.MkdirAll(filepath.Join(root, "mnt/disks/usb/films"), 0o755)
	os.WriteFile(filepath.Join(root, "mnt/disks/usb/films/b.mkv"), []byte("other b"), 0o644)
	run(TransferRequest{Op: "move", Paths: []string{"/mnt/user/media/films"}, Dest: "/mnt/disks/usb"})
	if read("mnt/disks/usb/films/a.mkv") != "film a, new cut" || read("mnt/disks/usb/films/b.mkv") != "other b" {
		t.Fatal("move merge")
	}
	if _, err := os.Stat(filepath.Join(root, "mnt/user/media/films/a.mkv")); err == nil {
		t.Fatal("moved file still at the source")
	}
	if read("mnt/user/media/films/b.mkv") != "film bb" {
		t.Fatal("the skipped file must stay at the source")
	}

	// A plain move is a rename.
	run(TransferRequest{Op: "move", Paths: []string{"/mnt/user/media/notes.txt"}, Dest: "/mnt/user/backup"})
	if read("mnt/user/backup/notes.txt") != "notes" {
		t.Fatal("rename move")
	}

	// No temporary files left anywhere.
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if strings.HasPrefix(d.Name(), ".hs-part-") {
			t.Errorf("left behind: %s", p)
		}
		return nil
	})
	if read("etc/passwd") != "root:x:0:0" {
		t.Fatal("passwd changed")
	}
}

func TestTransferStops(t *testing.T) {
	a, root := transferAgent(t)
	os.WriteFile(filepath.Join(root, "mnt/user/media/big.bin"), make([]byte, 1<<20), 0o644)
	p, err := a.planTransfer(context.Background(), TransferRequest{Op: "copy", Paths: []string{"/mnt/user/media/big.bin"}, Dest: "/mnt/user/backup"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.runTransfer(ctx, p, false, func(string) {}); err == nil {
		t.Fatal("a stopped transfer must say so")
	}
	des, _ := os.ReadDir(filepath.Join(root, "mnt/user/backup"))
	if len(des) != 0 {
		t.Fatalf("a stopped copy left %v", des)
	}
}
