package agent

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGz(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	_, _ = gz.Write([]byte(text))
	gz.Close()
	f.Close()
}

func TestRunLogs(t *testing.T) {
	dir := t.TempDir()
	a := New(Options{StateDir: dir, LogDir: dir, LockFile: filepath.Join(dir, "lock")})
	runs := a.runsDir()
	os.MkdirAll(runs, 0o755)

	// An old run, compressed, with its result; a newer one started from
	// Homestack that recorded nothing; a parity sync; and a stray file.
	writeGz(t, filepath.Join(runs, "mover-20261001-030000.log.gz"), "Mover started\n== media: 3 files to move\n== OK: Moved 3 files\n")
	os.WriteFile(filepath.Join(runs, "mover-20261001-030000.json"), []byte(`{"finished":"2026-10-01T03:05:00Z","ok":true,"summary":"Moved 3 files"}`), 0o644)
	os.WriteFile(filepath.Join(runs, "mover-20261002-030000-0123456789ab.log"), []byte("Mover started\n"), 0o644)
	os.WriteFile(filepath.Join(runs, "parity-sync-20261002-043000.log"), []byte("Parity sync started\n10%, 100 MB, 50 MB/s\n20%, 200 MB, 50 MB/s\n30%, 300 MB, 50 MB/s, 1:10 ETA\nEverything OK\n"), 0o644)
	os.WriteFile(filepath.Join(runs, "notes.txt"), []byte("x"), 0o644)

	movers := a.listRuns("mover")
	if len(movers) != 2 {
		t.Fatalf("got %d mover runs: %+v", len(movers), movers)
	}
	if movers[0].JobID != "0123456789ab" || movers[0].OK != nil || movers[0].Running {
		t.Errorf("newest run %+v", movers[0])
	}
	if movers[1].OK == nil || !*movers[1].OK || movers[1].Summary != "Moved 3 files" || !movers[1].Updated.Equal(time.Date(2026, 10, 1, 3, 5, 0, 0, time.UTC)) {
		t.Errorf("older run %+v", movers[1])
	}
	if all := a.listRuns(""); len(all) != 3 {
		t.Errorf("all runs: %d", len(all))
	}

	text, err := readRunLog(filepath.Join(runs, "mover-20261001-030000.log.gz"), 100)
	if err != nil || !strings.Contains(text, "3 files to move") {
		t.Errorf("gz log %q %v", text, err)
	}
	text, _ = readRunLog(filepath.Join(runs, "parity-sync-20261002-043000.log"), 100)
	if strings.Count(text, "%,") != 1 || !strings.Contains(text, "30%") {
		t.Errorf("progress lines not collapsed:\n%s", text)
	}

	// A job that ended without its script recording a result gets one.
	if a.runLogForJob("0123456789ab") == "" {
		t.Fatal("run log for job not found")
	}
	now := time.Now()
	a.finishRunLog(&Job{ID: "0123456789ab", State: "stopped", Detail: "Stopped by you.", Finished: &now})
	if r := a.listRuns("mover")[0]; r.OK == nil || *r.OK || r.Summary != "Stopped by you." {
		t.Errorf("after finishRunLog %+v", r)
	}

	// The API only serves run logs, by name.
	for name, want := range map[string]int{
		"mover-20261001-030000.log.gz": 200, "../jobs.json": 400, "mover-1.log": 400,
		"parity-check-20261001-030000.log": 404,
	} {
		rec := httptest.NewRecorder()
		a.getRunLog(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/log?name="+name, nil))
		if rec.Code != want {
			t.Errorf("%s: got %d, want %d", name, rec.Code, want)
		}
	}
}

func TestTailBytes(t *testing.T) {
	b, cut := tailBytes(strings.NewReader(strings.Repeat("a", 1000)+"END"), 10)
	if string(b) != "aaaaaaaEND" || !cut {
		t.Fatalf("got %q %v", b, cut)
	}
	if b, cut := tailBytes(strings.NewReader("short"), 10); string(b) != "short" || cut {
		t.Fatalf("got %q %v", b, cut)
	}
}

func TestMoverAge(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "homestack-alerts")
	os.WriteFile(conf, []byte("MOVER_SHARES=\"Media isos\"\nMOVER_AGE_MIN=1440\n"), 0o644)
	t.Setenv("HOMESTACK_ALERTS_CONF", conf)
	a := New(Options{StateDir: dir, LogDir: dir})

	if v := a.moverView(); v["ageMinutes"] != 1440 || v["custom"] != false {
		t.Fatalf("default view %+v", v)
	}
	put := func(body string) int {
		rec := httptest.NewRecorder()
		a.putMover(rec, httptest.NewRequest(http.MethodPut, "/v1/mover", strings.NewReader(body)))
		return rec.Code
	}
	if put(`{"ageMinutes":-1}`) != 400 || put(`{"ageMinutes":99999999}`) != 400 {
		t.Fatal("bad ages accepted")
	}
	if put(`{"ageMinutes":180}`) != 200 {
		t.Fatal("put failed")
	}
	b, _ := os.ReadFile(a.moverSharesFile())
	if !strings.Contains(string(b), "MOVER_AGE_MIN=180\n") || strings.Contains(string(b), "MOVER_SHARES") {
		t.Fatalf("shares not managed, file:\n%s", b)
	}
	// Saving shares keeps the age.
	if err := a.writeMoverShares(sharesState{Shares: []Share{{Name: "Media", Path: "/mnt/user/Media", Mover: true}}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(a.moverSharesFile())
	if !strings.Contains(string(b), `MOVER_SHARES="Media isos"`) || !strings.Contains(string(b), "MOVER_AGE_MIN=180") {
		t.Fatalf("file:\n%s", b)
	}
	// 0 hands the age back to the alerts file.
	if put(`{"ageMinutes":0}`) != 200 {
		t.Fatal("reset failed")
	}
	b, _ = os.ReadFile(a.moverSharesFile())
	if strings.Contains(string(b), "MOVER_AGE_MIN") {
		t.Fatalf("age kept after reset:\n%s", b)
	}
}
