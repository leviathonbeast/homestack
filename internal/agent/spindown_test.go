package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const diskstats1 = `   8       0 sda 1000 0 80000 0 500 0 40000 0 0 0 0 0 0 0 0 0 0
   8      16 sdb 2000 0 160000 0 100 0 8000 0 0 0 0 0 0 0 0 0 0
 259       0 nvme0n1 5000 0 400000 0 900 0 72000 0 0 0 0 0 0 0 0 0 0
`

// sda reads 10 MiB in 5 s; sdb does nothing.
const diskstats2 = `   8       0 sda 1100 0 100480 0 500 0 40000 0 0 0 0 0 0 0 0 0 0
   8      16 sdb 2000 0 160000 0 100 0 8000 0 0 0 0 0 0 0 0 0 0
 259       0 nvme0n1 5000 0 400000 0 950 0 82240 0 0 0 0 0 0 0 0 0 0
`

func TestIOTracker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "diskstats")
	tr := newIOTracker()
	t0 := time.Now()
	os.WriteFile(path, []byte(diskstats1), 0o644)
	tr.sample(path, t0)
	os.WriteFile(path, []byte(diskstats2), 0o644)
	tr.sample(path, t0.Add(5*time.Second))

	disks := []Disk{{Name: "sda", Asleep: true}, {Name: "sdb"}, {Name: "nvme0n1"}}
	tr.fill(disks, t0.Add(5*time.Second))
	if disks[0].ReadBps != 20480*512/5 || disks[0].WriteBps != 0 || disks[0].Asleep {
		t.Errorf("sda: %+v (reading wakes it)", disks[0])
	}
	if disks[1].ReadBps != 0 || *disks[1].IdleSec != 5 || disks[1].ReadBytes != 160000*512 {
		t.Errorf("sdb: %+v", disks[1])
	}
	if disks[2].WriteBps != 10240*512/5 {
		t.Errorf("nvme: %+v", disks[2])
	}
	if tr.idleFor("sda", t0.Add(65*time.Second)) != time.Minute || tr.idleFor("sdb", t0.Add(65*time.Second)) != 65*time.Second || tr.idleFor("sdz", t0) != 0 {
		t.Error("idleFor")
	}
}

// Only SATA hard disks idle for the delay are spun down, and nothing while
// a storage job runs.
func TestSpinTick(t *testing.T) {
	dir := t.TempDir()
	r := &scriptRunner{out: map[string]string{
		"which hdparm":       "/usr/sbin/hdparm\n",
		"hdparm -C /dev/sdb": "\n/dev/sdb:\n drive state is:  active/idle\n",
		"hdparm -C /dev/sdc": "\n/dev/sdc:\n drive state is:  standby\n",
		"hdparm -y /dev/sdb": "\n/dev/sdb:\n issuing standby command\n",
	}}
	a := New(Options{StateDir: dir, LogDir: dir, Runner: r, LockFile: filepath.Join(dir, "lock")})
	a.disks.disks = []Disk{
		{Name: "sda", Role: "system", Rotational: true, Transport: "sata"},
		{Name: "sdb", Role: "disk1", Rotational: true, Transport: "sata"},
		{Name: "sdc", Role: "disk2", Rotational: true, Transport: "sata"},
		{Name: "sdd", Role: "cache", Rotational: false, Transport: "sata"},
		{Name: "sde", Role: "disk3", Rotational: true, Transport: "sata"}, // busy
		{Name: "nvme0n1", Role: "cache", Transport: "nvme"},
	}
	t0 := time.Now()
	for _, n := range []string{"sda", "sdb", "sdc", "sdd", "sde", "nvme0n1"} {
		a.io.active[n] = t0.Add(-2 * time.Hour)
	}
	a.io.active["sde"] = t0.Add(-5 * time.Minute)

	a.spinTick(context.Background(), t0)
	if slices.ContainsFunc(r.ran, func(s string) bool { return strings.HasPrefix(s, "hdparm") }) {
		t.Fatalf("no delay set, but ran %v", r.ran)
	}
	os.WriteFile(a.spinPath(), []byte(`{"delayMinutes":30}`), 0o640)
	// Not while a parity job runs; once it's paused, yes.
	a.jobs.running = &Job{ID: "x", Action: "parity-check", Title: "Parity check"}
	a.spinTick(context.Background(), t0)
	if slices.ContainsFunc(r.ran, func(s string) bool { return strings.HasPrefix(s, "hdparm -y") }) {
		t.Fatalf("spun down during a parity check: %v", r.ran)
	}
	a.jobs.running.Paused = &t0
	a.spinTick(context.Background(), t0)
	var spun []string
	for _, s := range r.ran {
		if strings.HasPrefix(s, "hdparm -y") {
			spun = append(spun, s)
		}
	}
	if !slices.Equal(spun, []string{"hdparm -y /dev/sdb"}) {
		t.Errorf("spun %v (ran %v)", spun, r.ran)
	}
	for _, d := range a.disks.snapshot() {
		if (d.Name == "sdb" || d.Name == "sdc") != d.Asleep {
			t.Errorf("%s asleep=%v", d.Name, d.Asleep)
		}
	}
}

func TestSpinSettings(t *testing.T) {
	dir := t.TempDir()
	a := New(Options{StateDir: dir, LogDir: dir, Runner: &scriptRunner{out: map[string]string{}}})
	put := func(body string) int {
		w := httptest.NewRecorder()
		a.putSpin(w, httptest.NewRequest(http.MethodPut, "/v1/spindown", strings.NewReader(body)))
		return w.Code
	}
	if put(`{"delayMinutes":7}`) != 400 || put(`{"delayMinutes":60}`) != 200 || a.loadSpin().DelayMinutes != 60 {
		t.Error("delays")
	}
	if v := a.spinView(context.Background()); !strings.Contains(v.Problem, "hdparm isn't installed") {
		t.Errorf("%+v", v)
	}
}

// Parity settings: set on the web page, else the alerts settings, else the
// defaults; written for snapraid-job.
func TestParitySettings(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "homestack-alerts")
	t.Setenv("HOMESTACK_ALERTS_CONF", conf)
	os.WriteFile(conf, []byte("SNAPRAID_SCRUB_PERCENT=12\nSNAPRAID_SCRUB_AGE=0\n"), 0o644)
	a := New(Options{StateDir: dir, LogDir: dir})
	v := a.parityView()
	if v.Values["SNAPRAID_SCRUB_PERCENT"] != 12 || v.Values["SNAPRAID_SCRUB_AGE"] != 0 || v.Values["SNAPRAID_UPDATE_LIMIT"] != 1000 || v.Custom {
		t.Fatalf("from the alerts file: %+v", v)
	}
	put := func(body string) (int, string) {
		w := httptest.NewRecorder()
		a.putParity(w, httptest.NewRequest(http.MethodPut, "/v1/parity", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := put(`{"values":{"SNAPRAID_SCRUB_PERCENT":0}}`); code != 400 || !strings.Contains(body, "between 1 and 100") {
		t.Errorf("0%%: %d %s", code, body)
	}
	if code, body := put(`{"values":{"SNAPRAID_SCRUB_PERCENT":4,"SNAPRAID_SCRUB_AGE":30,"SNAPRAID_DELETE_LIMIT":50}}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if a.deleteLimit() != 50 || a.updateLimit() != 1000 || a.parityValue("SNAPRAID_SCRUB_AGE") != 30 {
		t.Error("values")
	}
	sh := readText(filepath.Join(dir, "homestack-parity"))
	if !strings.Contains(sh, "SNAPRAID_SCRUB_PERCENT=4\n") || !strings.Contains(sh, "SNAPRAID_DELETE_LIMIT=50\n") || strings.Contains(sh, "UPDATE_LIMIT") {
		t.Errorf("file:\n%s", sh)
	}
	var back ParityView
	_, body := put(`{"values":{}}`)
	json.Unmarshal([]byte(body), &back)
	if back.Custom || back.Values["SNAPRAID_SCRUB_PERCENT"] != 12 || strings.Contains(readText(filepath.Join(dir, "homestack-parity")), "=") {
		t.Errorf("back to the alerts file: %+v", back)
	}
}
