package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"homestack/internal/fans"
	"homestack/internal/notify"
)

// Fan control in the style of Unraid's fan control plugins: each fan header
// follows the hottest of the temperatures you pick (CPU, board, NVMe, any
// disk or group of disks), along a curve, with a minimum speed. Sleeping
// disks are ignored; if every disk a fan follows is asleep, it runs at the
// bottom of its curve. If a temperature it needs can't be read at all, the
// fan goes to 100% until it can. When the agent stops, every header goes
// back to the BIOS's own control.

// FanView is one header on the Fans page.
type FanView struct {
	ID         string         `json:"id"`
	Chip       string         `json:"chip"`
	Index      int            `json:"index"`
	RPM        *int           `json:"rpm"`
	Percent    int            `json:"percent"`
	Mode       string         `json:"mode"`
	Controlled bool           `json:"controlled"`
	SourceTemp *float64       `json:"sourceTemp,omitempty"`
	Source     string         `json:"source,omitempty"`
	Reason     string         `json:"reason,omitempty"` // failsafe, asleep, test…
	Stalled    bool           `json:"stalled,omitempty"`
	SeenSpin   bool           `json:"seenSpin"`
	Config     fans.FanConfig `json:"config"`
}

// SensorView is one temperature.
type SensorView struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Kind   string   `json:"kind"`
	Temp   *float64 `json:"temp"`
	Asleep bool     `json:"asleep,omitempty"`
}

type channel struct {
	id, chip, dir string
	index         int
}

type fanState struct {
	lastPct   int
	lastTemp  float64
	missSince time.Time
	zeroCount int
	seenSpin  bool
	stalled   bool
	alerted   bool
	touched   bool
}

type fanController struct {
	a        *Agent
	mu       sync.Mutex
	cfg      fans.Config
	state    map[string]*fanState
	original map[string]int // pwmN_enable before we took over
	override *fans.Override
	views    []FanView
	sensors  []SensorView
}

func newFanController(a *Agent) *fanController {
	return &fanController{a: a, state: map[string]*fanState{}, original: map[string]int{}}
}

func (f *fanController) cfgPath() string { return filepath.Join(f.a.opt.StateDir, "fans.json") }
func (f *fanController) origPath() string {
	return filepath.Join(f.a.opt.StateDir, "pwm-original.json")
}

func readInt(p string) (int, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return v, err == nil
}

func writeInt(p string, v int) error { return os.WriteFile(p, []byte(strconv.Itoa(v)), 0o644) }

// hwmon lists chips by name. Chip IDs use the driver name ("nct6798"), with
// "@device" added only if two chips share a name, so IDs survive reboots
// (hwmon numbers don't).
func (f *fanController) hwmon() (chips []struct{ id, dir string }) {
	root := f.a.sys("/sys/class/hwmon")
	ents, _ := os.ReadDir(root)
	count := map[string]int{}
	type c struct{ name, dir, dev string }
	var all []c
	for _, e := range ents {
		dir := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(b))
		dev := ""
		if l, err := os.Readlink(filepath.Join(dir, "device")); err == nil {
			dev = filepath.Base(l)
		}
		all = append(all, c{name, dir, dev})
		count[name]++
	}
	sort.Slice(all, func(i, j int) bool { return all[i].dir < all[j].dir })
	for _, x := range all {
		id := x.name
		if count[x.name] > 1 {
			id = x.name + "@" + x.dev
		}
		chips = append(chips, struct{ id, dir string }{id, x.dir})
	}
	return chips
}

func (f *fanController) channels() []channel {
	var out []channel
	for _, c := range f.hwmon() {
		for i := 1; i <= 16; i++ {
			if _, err := os.Stat(filepath.Join(c.dir, fmt.Sprintf("pwm%d", i))); err == nil {
				if _, err := os.Stat(filepath.Join(c.dir, fmt.Sprintf("pwm%d_enable", i))); err == nil {
					out = append(out, channel{id: fmt.Sprintf("%s/pwm%d", c.id, i), chip: c.id, dir: c.dir, index: i})
				}
			}
		}
	}
	return out
}

func sensorKind(chip string) string {
	switch {
	case chip == "k10temp" || chip == "coretemp" || chip == "zenpower":
		return "cpu"
	case strings.HasPrefix(chip, "nvme"):
		return "nvme"
	case chip == "amdgpu" || chip == "nouveau" || chip == "radeon" || chip == "i915":
		return "gpu"
	case strings.HasPrefix(chip, "nct") || strings.HasPrefix(chip, "it8") || strings.HasPrefix(chip, "f71"):
		return "board"
	case chip == "drivetemp":
		return "skip" // disks come from SMART instead
	}
	return "other"
}

// readSensors returns every temperature, plus disk and group sensors.
func (f *fanController) readSensors() ([]SensorView, map[string]SensorView) {
	var list []SensorView
	for _, c := range f.hwmon() {
		kind := sensorKind(strings.SplitN(c.id, "@", 2)[0])
		if kind == "skip" {
			continue
		}
		for i := 1; i <= 32; i++ {
			v, ok := readInt(filepath.Join(c.dir, fmt.Sprintf("temp%d_input", i)))
			if !ok {
				continue
			}
			t := float64(v) / 1000
			if t <= -40 || t >= 150 || t == 0 { // unconnected thermistors read nonsense
				continue
			}
			label := fmt.Sprintf("%s temp%d", c.id, i)
			if b, err := os.ReadFile(filepath.Join(c.dir, fmt.Sprintf("temp%d_label", i))); err == nil {
				label = strings.TrimSpace(string(b))
			}
			list = append(list, SensorView{ID: fmt.Sprintf("%s/temp%d", c.id, i), Label: label, Kind: kind, Temp: &t})
		}
	}
	// Disks (from SMART, never waking them) and groups.
	var arrayMax, hddMax, ssdMax *float64
	arrayAsleep, hddAsleep := true, true
	maxp := func(dst **float64, v float64) {
		if *dst == nil || v > **dst {
			x := v
			*dst = &x
		}
	}
	for _, d := range f.a.disks.snapshot() {
		s := SensorView{ID: "disk/" + d.Serial, Label: d.Label, Kind: "disk", Asleep: d.Asleep}
		if !d.Rotational {
			s.Kind = "ssd"
		}
		if d.Temp != nil {
			t := float64(*d.Temp)
			s.Temp = &t
			if d.Rotational {
				maxp(&hddMax, t)
				hddAsleep = false
				if strings.HasPrefix(d.Role, "disk") || d.Role == "parity" {
					maxp(&arrayMax, t)
					arrayAsleep = false
				}
			} else {
				maxp(&ssdMax, t)
			}
		}
		if d.Serial == "" {
			s.ID = "disk/" + d.Name
		}
		list = append(list, s)
	}
	list = append(list,
		SensorView{ID: "group/array", Label: "Hottest array disk", Kind: "group", Temp: arrayMax, Asleep: arrayMax == nil && arrayAsleep},
		SensorView{ID: "group/hdd", Label: "Hottest hard drive", Kind: "group", Temp: hddMax, Asleep: hddMax == nil && hddAsleep},
		SensorView{ID: "group/ssd", Label: "Hottest SSD", Kind: "group", Temp: ssdMax},
	)
	byID := map[string]SensorView{}
	for i := range list {
		if l, ok := f.cfg.Labels[list[i].ID]; ok && l != "" {
			list[i].Label = l
		}
		byID[list[i].ID] = list[i]
	}
	return list, byID
}

func (f *fanController) loadConfig() {
	c, err := fans.LoadConfig(f.cfgPath())
	if err == nil {
		f.cfg = c
	}
	b, err := os.ReadFile(f.origPath())
	if err == nil {
		_ = json.Unmarshal(b, &f.original)
	}
}

func (f *fanController) saveOriginal() {
	b, _ := json.Marshal(f.original)
	_ = os.WriteFile(f.origPath(), b, 0o640)
}

func (f *fanController) loop(ctx context.Context) {
	f.mu.Lock()
	f.loadConfig()
	f.mu.Unlock()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		f.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (f *fanController) cfgFor(id string) (fans.FanConfig, bool) {
	for _, c := range f.cfg.Fans {
		if c.ID == id {
			return c, true
		}
	}
	return fans.FanConfig{ID: id, Mode: fans.ModeBIOS}, false
}

func (f *fanController) tick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	sensors, byID := f.readSensors()
	now := time.Now()
	if f.override != nil && now.After(f.override.Until) {
		f.override = nil
	}
	var views []FanView
	for _, ch := range f.channels() {
		cfg, _ := f.cfgFor(ch.id)
		st := f.state[ch.id]
		if st == nil {
			st = &fanState{}
			f.state[ch.id] = st
		}
		v := FanView{ID: ch.id, Chip: ch.chip, Index: ch.index, Mode: cfg.Mode, Config: cfg}
		if rpm, ok := readInt(filepath.Join(ch.dir, fmt.Sprintf("fan%d_input", ch.index))); ok {
			v.RPM = &rpm
			if rpm > 0 {
				st.seenSpin = true
			}
		}
		target := -1
		switch {
		case f.override != nil && f.override.ID == ch.id:
			target, v.Reason = f.override.Percent, "test"
		case cfg.Mode == fans.ModeFixed:
			target = cfg.Percent
		case cfg.Mode == fans.ModeCurve:
			temp, src, state := sourceTemp(cfg.Sources, byID)
			switch state {
			case "ok":
				v.SourceTemp, v.Source = &temp, src
				st.missSince = time.Time{}
				target = fans.Interpolate(cfg.Curve, temp)
				// Hysteresis: only slow down once it's 2°C cooler than when we last sped up.
				if target < st.lastPct && temp > st.lastTemp-2 && st.touched {
					target = st.lastPct
				} else {
					st.lastTemp = temp
				}
			case "asleep":
				target, v.Reason = cfg.Curve[0].Percent, "asleep"
				st.missSince = time.Time{}
			default: // can't read a temperature this fan depends on
				if st.missSince.IsZero() {
					st.missSince = now
				}
				if now.Sub(st.missSince) > 60*time.Second || !st.touched {
					target, v.Reason = 100, "failsafe"
				} else {
					target = st.lastPct
				}
			}
		}
		if target >= 0 && target < cfg.MinPercent && v.Reason != "test" && v.Reason != "failsafe" {
			target = cfg.MinPercent
		}
		pwmPath := filepath.Join(ch.dir, fmt.Sprintf("pwm%d", ch.index))
		enPath := pwmPath + "_enable"
		if target >= 0 {
			target = max(0, min(100, target))
			if _, saved := f.original[ch.id]; !saved {
				if en, ok := readInt(enPath); ok {
					f.original[ch.id] = en
					f.saveOriginal()
				}
			}
			if en, _ := readInt(enPath); en != 1 {
				_ = writeInt(enPath, 1)
			}
			_ = writeInt(pwmPath, int(math.Round(float64(target)*255/100)))
			st.lastPct, st.touched = target, true
			v.Controlled = true
		} else if st.touched {
			f.restore(ch.id, enPath)
			st.touched = false
		}
		if raw, ok := readInt(pwmPath); ok {
			v.Percent = int(math.Round(float64(raw) * 100 / 255))
		}
		// Stall: told to spin, has spun before, now reads 0 RPM for ~20 s.
		if v.Controlled && v.Percent >= 30 && v.RPM != nil && *v.RPM == 0 && st.seenSpin {
			st.zeroCount++
		} else {
			st.zeroCount = 0
			if st.stalled {
				st.stalled, st.alerted = false, false
			}
		}
		if st.zeroCount >= 7 {
			st.stalled = true
			if !st.alerted {
				st.alerted = true
				name := cfg.Name
				if name == "" {
					name = ch.id
				}
				f.a.notifyHost(notify.Error, "Fan stopped: "+name, fmt.Sprintf("%s is set to %d%% but reads 0 RPM. Check it's plugged in and not blocked.", name, v.Percent))
			}
		}
		v.Stalled, v.SeenSpin = st.stalled, st.seenSpin
		views = append(views, v)
	}
	f.views, f.sensors = views, sensors
}

// sourceTemp returns the hottest readable source. state is "ok", "asleep"
// (every source is a sleeping disk) or "missing".
func sourceTemp(ids []string, byID map[string]SensorView) (float64, string, string) {
	best, src := math.Inf(-1), ""
	asleep, missing := 0, 0
	for _, id := range ids {
		s, ok := byID[id]
		switch {
		case ok && s.Temp != nil:
			if *s.Temp > best {
				best, src = *s.Temp, s.Label
			}
		case ok && s.Asleep:
			asleep++
		default:
			missing++
		}
	}
	switch {
	case src != "" && missing == 0:
		return best, src, "ok"
	case src != "":
		return best, src, "ok" // some sources unreadable, but we have a real reading
	case asleep > 0 && missing == 0:
		return 0, "", "asleep"
	}
	return 0, "", "missing"
}

func (f *fanController) restore(id, enPath string) {
	if en, ok := f.original[id]; ok {
		_ = writeInt(enPath, en)
	} else {
		_ = writeInt(enPath, 5) // NCT "SmartFan IV": the usual BIOS mode
	}
}

// restoreAll hands every header back to the BIOS (called on exit).
func (f *fanController) restoreAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.channels() {
		if _, ok := f.original[ch.id]; ok {
			f.restore(ch.id, filepath.Join(ch.dir, fmt.Sprintf("pwm%d_enable", ch.index)))
		}
	}
}

// RestoreFans is `homestack agent restore-fans`, run by systemd after the
// agent stops for any reason (even a crash).
func RestoreFans(opt Options) error {
	a := New(opt)
	b, err := os.ReadFile(a.fans.origPath())
	if err != nil {
		return nil
	}
	_ = json.Unmarshal(b, &a.fans.original)
	for _, ch := range a.fans.channels() {
		if en, ok := a.fans.original[ch.id]; ok {
			_ = writeInt(filepath.Join(ch.dir, fmt.Sprintf("pwm%d_enable", ch.index)), en)
		}
	}
	return nil
}

func (f *fanController) view() ([]FanView, []SensorView) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := append([]FanView(nil), f.views...)
	s := append([]SensorView(nil), f.sensors...)
	if v == nil {
		v = []FanView{}
	}
	if s == nil {
		s = []SensorView{}
	}
	return v, s
}

func (f *fanController) configView() fans.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cfg
	if c.Fans == nil {
		c.Fans = []fans.FanConfig{}
	}
	return c
}

func (f *fanController) putConfig(w http.ResponseWriter, r *http.Request) {
	var c fans.Config
	if err := readBody(r, &c); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if err := c.Validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	known := map[string]bool{}
	for _, ch := range f.channels() {
		known[ch.id] = true
	}
	for _, fc := range c.Fans {
		if !known[fc.ID] {
			writeErr(w, 400, "unknown fan header "+fc.ID)
			return
		}
	}
	if err := fans.WriteJSON(f.cfgPath(), c, 0o640); err != nil {
		writeErr(w, 500, "couldn't save")
		return
	}
	f.mu.Lock()
	f.cfg = c
	f.mu.Unlock()
	f.tick()
	writeJSON(w, 200, f.configView())
}

func (f *fanController) postTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Percent int    `json:"percent"`
		Seconds int    `json:"seconds"`
	}
	if err := readBody(r, &req); err != nil || req.Percent < 0 || req.Percent > 100 || req.Seconds < 1 || req.Seconds > 60 {
		writeErr(w, 400, "percent 0–100 and seconds 1–60, please")
		return
	}
	found := false
	for _, ch := range f.channels() {
		found = found || ch.id == req.ID
	}
	if !found {
		writeErr(w, 400, "unknown fan header")
		return
	}
	f.mu.Lock()
	f.override = &fans.Override{ID: req.ID, Percent: req.Percent, Until: time.Now().Add(time.Duration(req.Seconds) * time.Second)}
	f.mu.Unlock()
	f.tick()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
