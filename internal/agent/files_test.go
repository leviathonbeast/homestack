package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "mnt/disks/ssd")
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  app:\n    image: x\n"), 0o640)
	os.WriteFile(filepath.Join(dir, "win.txt"), []byte("one\r\ntwo\r\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte{0xff, 0xd8, 0, 1, 2}, 0o644)
	os.WriteFile(filepath.Join(dir, "big.log"), make([]byte, maxEditBytes+1), 0o644)
	os.WriteFile(filepath.Join(dir, "snapraid.content"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root:x:0:0"), 0o644)
	os.Symlink(filepath.Join(root, "etc/passwd"), filepath.Join(dir, "escape"))
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), LogDir: t.TempDir()})

	get := func(p string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		a.readFileContent(rec, httptest.NewRequest(http.MethodGet, "/v1/files/content?path="+p, nil))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	put := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		a.writeFileContent(rec, httptest.NewRequest(http.MethodPut, "/v1/files/content", strings.NewReader(string(b))))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, f := get("/mnt/disks/ssd/compose.yaml")
	if code != 200 || f["content"] != "services:\n  app:\n    image: x\n" || f["eol"] != "lf" {
		t.Fatalf("read: %d %v", code, f)
	}
	for _, p := range []string{"/mnt/disks/ssd/photo.jpg", "/mnt/disks/ssd/big.log", "/mnt/disks/ssd/snapraid.content", "/mnt/disks/ssd/escape", "/etc/passwd", "/mnt/disks/ssd"} {
		if code, _ := get(p); code == 200 {
			t.Errorf("%s: opened for editing", p)
		}
	}

	// Save keeps the file's mode; a stale stamp is refused unless forced.
	code, res := put(map[string]any{"path": "/mnt/disks/ssd/compose.yaml", "content": "services: {}\n", "stamp": f["stamp"]})
	if code != 200 {
		t.Fatalf("save: %d %v", code, res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(b) != "services: {}\n" {
		t.Fatalf("saved %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "compose.yaml")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	if code, res := put(map[string]any{"path": "/mnt/disks/ssd/compose.yaml", "content": "lost\n", "stamp": f["stamp"]}); code != 409 || res["conflict"] != true {
		t.Fatalf("stale save: %d %v", code, res)
	}
	if code, _ := put(map[string]any{"path": "/mnt/disks/ssd/compose.yaml", "content": "mine\n", "stamp": "x", "force": true}); code != 200 {
		t.Fatal("forced save refused")
	}

	// CRLF files are edited with \n and saved back with CRLF.
	_, w := get("/mnt/disks/ssd/win.txt")
	if w["content"] != "one\ntwo\n" || w["eol"] != "crlf" {
		t.Fatalf("crlf read %v", w)
	}
	put(map[string]any{"path": "/mnt/disks/ssd/win.txt", "content": "one\ntwo\nthree\n", "eol": "crlf", "stamp": w["stamp"]})
	if b, _ := os.ReadFile(filepath.Join(dir, "win.txt")); string(b) != "one\r\ntwo\r\nthree\r\n" {
		t.Fatalf("crlf saved %q", b)
	}

	// Writing through a symlink out of the storage is refused.
	if code, _ := put(map[string]any{"path": "/mnt/disks/ssd/escape", "content": "pwned", "force": true}); code == 200 {
		t.Fatal("wrote outside the storage")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/passwd")); string(b) != "root:x:0:0" {
		t.Fatal("passwd changed")
	}

	// New empty files.
	op := func(body string) int {
		rec := httptest.NewRecorder()
		a.fileOp(rec, httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(body)))
		return rec.Code
	}
	if op(`{"op":"mkfile","path":"/mnt/disks/ssd","name":"notes.md"}`) != 200 {
		t.Fatal("mkfile failed")
	}
	if op(`{"op":"mkfile","path":"/mnt/disks/ssd","name":"notes.md"}`) == 200 || op(`{"op":"mkfile","path":"/mnt/disks/ssd","name":"../x"}`) == 200 {
		t.Fatal("mkfile overwrote or escaped")
	}
	if code, f := get("/mnt/disks/ssd/notes.md"); code != 200 || f["content"] != "" {
		t.Fatalf("new file: %d %v", code, f)
	}
}
