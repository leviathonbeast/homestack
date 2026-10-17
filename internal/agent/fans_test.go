package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"homestack/internal/fans"
)

// fakeBoard builds /sys/class/hwmon with an NCT6798 (7 headers, fan2 and
// fan7 spinning), k10temp and nvme, like the real server.
func fakeBoard(t *testing.T) string {
	root := t.TempDir()
	mk := func(dir string, files map[string]string) {
		p := filepath.Join(root, "sys/class/hwmon", dir)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range files {
			os.WriteFile(filepath.Join(p, k), []byte(v+"\n"), 0o644)
		}
	}
	nct := map[string]string{"name": "nct6798", "temp1_input": "31000", "temp1_label": "SYSTIN", "temp2_input": "-61000"}
	for i := 1; i <= 7; i++ {
		s := strconv.Itoa(i)
		nct["pwm"+s] = "128"
		nct["pwm"+s+"_enable"] = "5"
		nct["fan"+s+"_input"] = "0"
	}
	nct["fan2_input"] = "1511"
	nct["fan7_input"] = "2132"
	mk("hwmon3", nct)
	mk("hwmon1", map[string]string{"name": "k10temp", "temp1_input": "48600", "temp1_label": "Tctl"})
	mk("hwmon0", map[string]string{"name": "nvme", "temp1_input": "41000", "temp1_label": "Composite"})
	return root
}

func rd(t *testing.T, root, f string) int {
	b, err := os.ReadFile(filepath.Join(root, "sys/class/hwmon/hwmon3", f))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return v
}

func newTestAgent(t *testing.T, root string) *Agent {
	a := New(Options{SysRoot: root, StateDir: t.TempDir(), LogDir: t.TempDir()})
	return a
}

func setDisks(a *Agent, temps map[string]int, asleep map[string]bool) {
	var ds []Disk
	for role, t := range temps {
		ds = append(ds, Disk{Name: "sd" + role, Serial: "SER" + role, Role: role, Label: role, Rotational: true, Temp: &t})
	}
	for role := range asleep {
		ds = append(ds, Disk{Name: "sd" + role, Serial: "SER" + role, Role: role, Label: role, Rotational: true, Asleep: true})
	}
	a.disks.mu.Lock()
	a.disks.disks = ds
	a.disks.mu.Unlock()
}

func TestFanDiscoveryAndDefaults(t *testing.T) {
	root := fakeBoard(t)
	a := newTestAgent(t, root)
	a.fans.tick()
	v, s := a.fans.view()
	if len(v) != 7 || v[1].ID != "nct6798/pwm2" || *v[1].RPM != 1511 || !v[1].SeenSpin || v[0].SeenSpin {
		t.Fatalf("fans: %+v", v)
	}
	// Nothing configured: BIOS keeps control, nothing written.
	if rd(t, root, "pwm2_enable") != 5 || v[1].Controlled {
		t.Fatal("touched a fan with no config")
	}
	ids := map[string]bool{}
	for _, x := range s {
		ids[x.ID] = true
	}
	if !ids["k10temp/temp1"] || !ids["nct6798/temp1"] || ids["nct6798/temp2"] || !ids["group/array"] {
		t.Fatalf("sensors: %v", ids)
	}
}

func TestFanCurveHysteresisAndRestore(t *testing.T) {
	root := fakeBoard(t)
	a := newTestAgent(t, root)
	setDisks(a, map[string]int{"disk1": 40, "disk2": 38}, nil)
	a.fans.cfg = fans.Config{Fans: []fans.FanConfig{{
		ID: "nct6798/pwm7", Name: "Disk fans", Mode: fans.ModeCurve, Sources: []string{"group/array"},
		Curve: []fans.Point{{Temp: 35, Percent: 30}, {Temp: 45, Percent: 100}}, MinPercent: 25,
	}}}
	a.fans.tick()
	// 40°C is halfway: 65% -> pwm 166
	if rd(t, root, "pwm7_enable") != 1 || rd(t, root, "pwm7") != 166 {
		t.Fatalf("enable=%d pwm=%d", rd(t, root, "pwm7_enable"), rd(t, root, "pwm7"))
	}
	// Cools by 1°C: hysteresis keeps 65%.
	setDisks(a, map[string]int{"disk1": 39, "disk2": 38}, nil)
	a.fans.tick()
	if rd(t, root, "pwm7") != 166 {
		t.Fatalf("hysteresis: pwm=%d", rd(t, root, "pwm7"))
	}
	// Cools by 3°C: slows down (37°C = 44%).
	setDisks(a, map[string]int{"disk1": 37, "disk2": 36}, nil)
	a.fans.tick()
	if got := rd(t, root, "pwm7"); got != 112 {
		t.Fatalf("after cooling pwm=%d want 112", got)
	}
	// All disks asleep: bottom of the curve (30%), not full speed.
	setDisks(a, nil, map[string]bool{"disk1": true, "disk2": true})
	a.fans.tick()
	if got := rd(t, root, "pwm7"); got != 77 {
		t.Fatalf("asleep pwm=%d want 77", got)
	}
	// Back to BIOS on exit, with the original mode.
	a.fans.restoreAll()
	if rd(t, root, "pwm7_enable") != 5 {
		t.Fatal("not restored")
	}
	// And the systemd ExecStopPost path works from the saved file.
	os.WriteFile(filepath.Join(root, "sys/class/hwmon/hwmon3/pwm7_enable"), []byte("1"), 0o644)
	if err := RestoreFans(Options{SysRoot: root, StateDir: a.opt.StateDir, LogDir: a.opt.LogDir}); err != nil || rd(t, root, "pwm7_enable") != 5 {
		t.Fatal("RestoreFans didn't restore")
	}
}

func TestFanFailSafeAndMinimum(t *testing.T) {
	root := fakeBoard(t)
	a := newTestAgent(t, root)
	a.fans.cfg = fans.Config{Fans: []fans.FanConfig{
		{ID: "nct6798/pwm2", Mode: fans.ModeCurve, Sources: []string{"disk/SERgone"}, Curve: []fans.Point{{Temp: 30, Percent: 20}, {Temp: 50, Percent: 80}}},
		{ID: "nct6798/pwm1", Mode: fans.ModeFixed, Percent: 10, MinPercent: 35},
	}}
	a.fans.tick()
	if rd(t, root, "pwm2") != 255 { // a sensor it depends on doesn't exist: full speed
		t.Fatalf("failsafe pwm=%d", rd(t, root, "pwm2"))
	}
	v, _ := a.fans.view()
	if v[1].Reason != "failsafe" {
		t.Fatalf("reason %q", v[1].Reason)
	}
	if rd(t, root, "pwm1") != 89 { // fixed 10% raised to the 35% minimum
		t.Fatalf("min pwm=%d", rd(t, root, "pwm1"))
	}
	// Switching a fan back to BIOS mode hands it back.
	a.fans.cfg.Fans = a.fans.cfg.Fans[:1]
	a.fans.tick()
	if rd(t, root, "pwm1_enable") != 5 {
		t.Fatal("fan not handed back to BIOS")
	}
}

func TestFanStallDetection(t *testing.T) {
	root := fakeBoard(t)
	a := newTestAgent(t, root)
	a.fans.cfg = fans.Config{Fans: []fans.FanConfig{{ID: "nct6798/pwm2", Mode: fans.ModeFixed, Percent: 60}}}
	a.fans.tick() // seen spinning at 1511
	os.WriteFile(filepath.Join(root, "sys/class/hwmon/hwmon3/fan2_input"), []byte("0"), 0o644)
	for range 8 {
		a.fans.tick()
	}
	v, _ := a.fans.view()
	if !v[1].Stalled {
		t.Fatal("stall not detected")
	}
}

func TestSnapStatusParse(t *testing.T) {
	out := `SnapRAID status report:

   Files Fragmented Excess  Wasted  Used    Free  Use Name
            Files  Fragments  GB      GB      GB
   12345       2       5       0.0   10900    3620  75% d1

The oldest block was scrubbed 12 days ago, the median 5, the newest 0.
No sync is in progress.
92% of the array is not scrubbed.
No rehash is in progress or needed.
No error detected.
`
	sum, errs := parseSnapStatus(out)
	if errs != "" || len(sum) != 5 {
		t.Fatalf("sum=%q errs=%q", sum, errs)
	}
	_, errs = parseSnapStatus("DANGER! In the array there are 3 errors!\n")
	if !strings.HasPrefix(errs, "DANGER") {
		t.Fatal("danger not caught")
	}
}

func TestSnapProgressLine(t *testing.T) {
	var j Job
	mm := reSnapProgress.FindStringSubmatch("4%, 1814528 MB, 556 MB/s, 2210 stripe/s, CPU 12%, 28:45 ETA")
	if mm == nil {
		t.Fatal("no match")
	}
	applySnapProgress(&j, mm)
	if *j.Progress != 4 || *j.Done != 1814528<<20 || *j.ETASecs != 28*3600+45*60 || j.Speed != "556 MB/s" || j.Total != nil {
		t.Fatalf("%+v (no total guessed from SnapRAID's percentage)", j)
	}
}
