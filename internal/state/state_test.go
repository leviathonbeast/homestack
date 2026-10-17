package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "homestack.json")
	st, err := Open(path, "/opt/stacks")
	if err != nil {
		t.Fatal(err)
	}
	backup, _ := json.Marshal(State{
		Version:  1,
		Admin:    &Admin{Username: "andrew", PasswordHash: "$argon2id$x"},
		Sessions: []Session{{ID: "old", TokenHash: "abc"}},
		Settings: Settings{StacksDir: "/mnt/user/appdata/stacks", AppHost: "lucy.lan"},
	})
	if err := st.Restore(backup); err != nil {
		t.Fatal(err)
	}
	st.Read(func(s *State) {
		if s.Admin == nil || s.Admin.Username != "andrew" || len(s.Sessions) != 0 || s.Settings.StacksDir != "/mnt/user/appdata/stacks" || s.Settings.SessionDays != 30 {
			t.Fatalf("restored: %+v", s)
		}
	})
	// Saved, so it survives a restart.
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"username": "andrew"`) || strings.Contains(string(b), "abc") {
		t.Fatalf("file:\n%s", b)
	}

	// Without an account it isn't a backup of a working install: refused,
	// and nothing changes.
	for _, bad := range []string{`{"version":1}`, `not json`, `{"version":1,"admin":{"username":"x"}}`} {
		if err := st.Restore([]byte(bad)); err == nil {
			t.Errorf("restored %s", bad)
		}
	}
	st.Read(func(s *State) {
		if s.Admin == nil || s.Admin.Username != "andrew" {
			t.Fatal("a refused restore changed the state")
		}
	})
}
