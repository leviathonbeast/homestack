package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homestack/internal/backup"
	"homestack/internal/state"
)

const testPass = "correct horse battery staple"

func testBackupFile(t *testing.T) string {
	t.Helper()
	k, err := backup.NewKey(testPass)
	if err != nil {
		t.Fatal(err)
	}
	hs, _ := json.Marshal(state.State{Version: 1, Admin: &state.Admin{Username: "andrew", PasswordHash: "$argon2id$x"}})
	m := backup.Manifest{Format: 1, Created: time.Now(), Host: "lucy", Version: "0.18.0", Parts: []backup.Part{
		{ID: "storage", Files: []string{"storage/snapraid.conf"}}, {ID: "homestack", Files: []string{"homestack/homestack.json"}}}}
	plain, err := backup.Pack(m, []backup.Entry{{Name: "storage/snapraid.conf", Data: []byte("data d1 /mnt/disk1/\n")}, {Name: "homestack/homestack.json", Data: hs}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := backup.Seal(k, plain)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sealed)
}

func call(h http.HandlerFunc, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/", bytes.NewReader(b)))
	return w
}

// A fresh install, before the host agent is there: the backup opens with
// its passphrase, only Homestack's own settings can come back, and after
// that the restore needs signing in.
func TestRestoreAtSetup(t *testing.T) {
	t.Setenv("HOMESTACK_AGENT_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	backupMu.Lock()
	backupOpen, backupFails = nil, nil
	backupMu.Unlock()
	st, err := state.Open(filepath.Join(t.TempDir(), "homestack.json"), "/opt/stacks")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	file := testBackupFile(t)
	open, restore := s.setupOrAuth(s.openBackup), s.setupOrAuth(s.restoreBackup)

	if w := call(open, map[string]string{"file": file, "passphrase": "wrong horse battery"}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "passphrase doesn't open") {
		t.Fatalf("wrong passphrase: %d %s", w.Code, w.Body)
	}
	w := call(open, map[string]string{"file": file, "passphrase": testPass})
	if w.Code != http.StatusOK {
		t.Fatalf("open: %d %s", w.Code, w.Body)
	}
	var v openedView
	json.Unmarshal(w.Body.Bytes(), &v)
	if v.Host != "lucy" || v.Agent || len(v.Parts) != 2 || v.Parts[0].ID != "storage" || v.Parts[0].Available || v.Parts[0].Why == "" || !v.Parts[1].Available || !v.Parts[1].Default {
		t.Fatalf("view: %+v", v)
	}

	// The array needs the agent.
	if w := call(restore, map[string]any{"token": v.Token, "parts": []string{"storage"}}); w.Code != http.StatusConflict {
		t.Fatalf("storage without the agent: %d %s", w.Code, w.Body)
	}
	if w := call(restore, map[string]any{"token": "not-it", "parts": []string{"homestack"}}); w.Code != http.StatusConflict {
		t.Fatalf("wrong token: %d", w.Code)
	}
	w = call(restore, map[string]any{"token": v.Token, "parts": []string{"homestack"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signedOut":true`) {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	st.Read(func(s *state.State) {
		if s.Admin == nil || s.Admin.Username != "andrew" {
			t.Fatalf("account not restored: %+v", s.Admin)
		}
	})
	// Not twice: the account exists now, so it takes signing in, and the
	// opened backup (it held secrets) is gone anyway.
	if w := call(restore, map[string]any{"token": v.Token, "parts": []string{"homestack"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("restored twice: %d", w.Code)
	}
	backupMu.Lock()
	if backupOpen != nil {
		t.Error("the opened backup was kept after restoring")
	}
	backupMu.Unlock()
	// Now there's an account: no more restoring without signing in.
	if w := call(open, map[string]string{"file": file, "passphrase": testPass}); w.Code != http.StatusUnauthorized {
		t.Fatalf("open after setup without a session: %d", w.Code)
	}
	// Signed in, Homestack's own settings aren't ticked to begin with:
	// they'd replace the ones in use.
	w = call(s.openBackup, map[string]string{"file": file, "passphrase": testPass})
	json.Unmarshal(w.Body.Bytes(), &v)
	if w.Code != http.StatusOK || v.Parts[1].ID != "homestack" || v.Parts[1].Default {
		t.Fatalf("running install: %d %+v", w.Code, v.Parts)
	}
}

func TestBackupOpenLimits(t *testing.T) {
	t.Setenv("HOMESTACK_AGENT_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	backupMu.Lock()
	backupOpen, backupFails = nil, nil
	backupMu.Unlock()
	st, _ := state.Open(filepath.Join(t.TempDir(), "homestack.json"), "/opt/stacks")
	s := &Server{store: st}
	file := testBackupFile(t)

	// An opened backup expires.
	w := call(s.openBackup, map[string]string{"file": file, "passphrase": testPass})
	var v openedView
	json.Unmarshal(w.Body.Bytes(), &v)
	backupMu.Lock()
	backupOpen.expires = time.Now().Add(-time.Second)
	backupMu.Unlock()
	if w := call(s.restoreBackup, map[string]any{"token": v.Token, "parts": []string{"homestack"}}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "half an hour") {
		t.Fatalf("expired: %d %s", w.Code, w.Body)
	}

	// Ten wrong passphrases, then a pause, even for the right one.
	for range 10 {
		call(s.openBackup, map[string]string{"file": file, "passphrase": "guess guess guess"})
	}
	if w := call(s.openBackup, map[string]string{"file": file, "passphrase": testPass}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("no pause after 10 wrong passphrases: %d", w.Code)
	}
	if w := call(s.openBackup, map[string]string{"file": base64.StdEncoding.EncodeToString([]byte("PK zip")), "passphrase": testPass}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("still paused: %d", w.Code)
	}
	backupMu.Lock()
	backupFails = nil
	backupMu.Unlock()
	if w := call(s.openBackup, map[string]string{"file": base64.StdEncoding.EncodeToString([]byte("PK zip")), "passphrase": testPass}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "isn't a Homestack backup") {
		t.Fatalf("not a backup: %d %s", w.Code, w.Body)
	}
}
