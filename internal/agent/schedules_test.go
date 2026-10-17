package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduleImportRenderRoundTrip(t *testing.T) {
	shipped, err := os.ReadFile("../../deploy/debian/notify/homestack-alerts.cron")
	if err != nil {
		t.Fatal(err)
	}
	got := importCron(string(shipped))
	want := map[string]string{"disk-temp": "*/10 * * * *", "space": "17 * * * *", "smart": "37 * * * *", "mover": "0 3 * * *", "parity-sync": "30 4 * * *", "parity-check": "0 6 * * 0"}
	for id, c := range want {
		if got[id].cron() != c {
			t.Errorf("%s: %q want %q", id, got[id].cron(), c)
		}
	}
	if !got["mover"].Enabled || got["parity-sync"].Enabled || got["parity-check"].Enabled {
		t.Fatalf("enabled flags wrong: %+v", got)
	}

	dir := t.TempDir()
	t.Setenv("HOMESTACK_AGENT_CRON", filepath.Join(dir, "homestack-alerts"))
	a := New(Options{StateDir: dir, LogDir: dir, ScriptsDir: dir})
	if err := a.writeCron(got); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.cronFile())
	if !strings.Contains(string(b), cronHeader) || !strings.Contains(string(b), "#30 4 * * *") {
		t.Fatalf("cron file:\n%s", b)
	}
	again := importCron(string(b))
	for id := range want {
		if again[id].cron() != got[id].cron() || again[id].Enabled != got[id].Enabled {
			t.Errorf("%s changed on round trip", id)
		}
	}
	if fi, _ := os.Stat(a.cronFile()); fi.Mode().Perm() != 0o644 {
		t.Fatal("cron file must be 0644")
	}
}

func TestScheduleValidateAndNext(t *testing.T) {
	smart := taskByID("smart")
	s := Schedule{ID: "smart", Every: "minutes", Interval: 5}
	if s.validate(smart) == nil {
		t.Fatal("SMART every 5 minutes should be refused")
	}
	s = Schedule{ID: "mover", Every: "monthly", Day: 31, Hour: 3}
	if s.validate(taskByID("mover")) == nil {
		t.Fatal("day 31 should be refused")
	}
	s = Schedule{ID: "parity-check", Every: "weekly", Hour: 6, Weekdays: []int{3, 0, 3}}
	if err := s.validate(taskByID("parity-check")); err != nil || len(s.Weekdays) != 2 || s.cron() != "0 6 * * 0,3" {
		t.Fatalf("weekly: %v %+v %q", err, s, s.cron())
	}
	from := time.Date(2026, 9, 25, 13, 58, 30, 0, time.Local) // a Friday
	if n := s.next(from); n == nil || n.Weekday() != time.Sunday || n.Hour() != 6 {
		t.Fatalf("next %v", n)
	}
	d := Schedule{Every: "minutes", Interval: 10}
	if n := d.next(from); n == nil || n.Minute() != 0 || n.Hour() != 14 {
		t.Fatalf("next %v", n)
	}
}

func TestScheduleEnableNeedsScripts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOMESTACK_AGENT_CRON", filepath.Join(dir, "homestack-alerts"))
	a := New(Options{StateDir: dir, LogDir: dir, ScriptsDir: dir})
	if why := a.blockedReason(taskByID("mover")); !strings.Contains(why, "Not installed") {
		t.Fatalf("got %q", why)
	}
	os.WriteFile(filepath.Join(dir, "snapraid-job"), []byte("#!/bin/sh\n"), 0o755)
	if why := a.blockedReason(taskByID("parity-sync")); !strings.Contains(why, "SnapRAID") {
		t.Fatalf("parity sync should wait for SnapRAID, got %q", why)
	}
}
