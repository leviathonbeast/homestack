// Package fans controls case fans from temperatures: CPU, board, NVMe and
// each hard drive. The control loop runs as its own service on the host
// (`homestack fans run`), so fans keep following their curves even while
// Homestack's container is stopped or being updated. Homestack's Fans page
// edits fans.json and reads fans-status.json, both in Homestack's data folder.
package fans

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// File names inside Homestack's data folder.
const (
	ConfigFile   = "fans.json"
	StatusFile   = "fans-status.json"
	OverrideFile = "fans-override.json"
)

// Modes for a fan header.
const (
	ModeBIOS  = "bios"  // leave it to the motherboard (the default: nothing is touched)
	ModeFixed = "fixed" // a set speed
	ModeCurve = "curve" // follow a temperature
)

// Point is one point on a fan curve.
type Point struct {
	Temp    float64 `json:"temp"`
	Percent int     `json:"percent"`
}

// FanConfig is the setting for one fan header.
type FanConfig struct {
	ID         string   `json:"id"` // "nct6798/pwm2"
	Name       string   `json:"name,omitempty"`
	Mode       string   `json:"mode"`
	Percent    int      `json:"percent,omitempty"`    // fixed mode
	Sources    []string `json:"sources,omitempty"`    // curve mode: the hottest of these is used
	Curve      []Point  `json:"curve,omitempty"`      // curve mode
	MinPercent int      `json:"minPercent,omitempty"` // never slower than this while controlled
}

// Config is fans.json.
type Config struct {
	Fans []FanConfig `json:"fans"`
	// Labels names temperature sensors, e.g. {"nct6798/temp1": "Case"}.
	Labels map[string]string `json:"labels,omitempty"`
}

// Override temporarily sets one fan's speed (the Test button).
type Override struct {
	ID      string    `json:"id"`
	Percent int       `json:"percent"`
	Until   time.Time `json:"until"`
}

// Validate checks a config before it's saved.
// Safety floors: a fixed speed can't stop a fan, and every curve must end
// fast enough to cool hot disks.
const (
	MinFixed    = 20
	MinCurveTop = 50
)

func (c *Config) Validate() error {
	seen := map[string]bool{}
	for i := range c.Fans {
		f := &c.Fans[i]
		if f.ID == "" || len(f.ID) > 100 || seen[f.ID] {
			return errors.New("each fan needs a unique id")
		}
		seen[f.ID] = true
		if len(f.Name) > 40 {
			return fmt.Errorf("%s: keep the name under 40 characters", f.ID)
		}
		if f.MinPercent < 0 || f.MinPercent > 100 {
			return fmt.Errorf("%s: minimum speed must be 0–100%%", f.ID)
		}
		switch f.Mode {
		case ModeBIOS:
		case ModeFixed:
			if f.Percent < MinFixed || f.Percent > 100 {
				return fmt.Errorf("%s: a fixed speed must be %d–100%% (many fans stop below that; use BIOS mode to let the board decide)", nameOr(f), MinFixed)
			}
		case ModeCurve:
			if len(f.Sources) == 0 {
				return fmt.Errorf("%s: pick at least one temperature for the curve to follow", nameOr(f))
			}
			if len(f.Curve) < 2 || len(f.Curve) > 10 {
				return fmt.Errorf("%s: a curve needs 2 to 10 points", nameOr(f))
			}
			sort.Slice(f.Curve, func(a, b int) bool { return f.Curve[a].Temp < f.Curve[b].Temp })
			for j, p := range f.Curve {
				if p.Percent < 0 || p.Percent > 100 || p.Temp < 0 || p.Temp > 120 {
					return fmt.Errorf("%s: curve points must be 0–120°C and 0–100%%", nameOr(f))
				}
				if j > 0 && p.Temp == f.Curve[j-1].Temp {
					return fmt.Errorf("%s: two curve points have the same temperature", nameOr(f))
				}
			}
			if top := f.Curve[len(f.Curve)-1].Percent; top < MinCurveTop {
				return fmt.Errorf("%s: the hottest point of the curve must be at least %d%%, so the fan speeds up when things get hot", nameOr(f), MinCurveTop)
			}
		default:
			return fmt.Errorf("%s: unknown mode %q", f.ID, f.Mode)
		}
	}
	for k, v := range c.Labels {
		if len(k) > 100 || len(v) > 40 {
			return errors.New("sensor names must be under 40 characters")
		}
	}
	return nil
}

func nameOr(f *FanConfig) string {
	if f.Name != "" {
		return f.Name
	}
	return f.ID
}

// Interpolate returns the speed for a temperature on a curve (sorted by temp).
func Interpolate(curve []Point, t float64) int {
	if len(curve) == 0 {
		return 100
	}
	if t <= curve[0].Temp {
		return curve[0].Percent
	}
	for i := 1; i < len(curve); i++ {
		a, b := curve[i-1], curve[i]
		if t <= b.Temp {
			f := (t - a.Temp) / (b.Temp - a.Temp)
			return int(math.Round(float64(a.Percent) + f*float64(b.Percent-a.Percent)))
		}
	}
	return curve[len(curve)-1].Percent
}

// ---------- Status (written by the service, read by Homestack) ----------

// FanStatus is the live state of one header.
type FanStatus struct {
	ID         string   `json:"id"`
	Chip       string   `json:"chip"`
	Index      int      `json:"index"`
	RPM        *int     `json:"rpm"` // nil = no tach input
	Percent    int      `json:"percent"`
	Mode       string   `json:"mode"`
	Enable     int      `json:"enable"` // pwmN_enable as read (1 = manual)
	SourceTemp *float64 `json:"sourceTemp,omitempty"`
	Source     string   `json:"source,omitempty"`
	FailSafe   bool     `json:"failSafe,omitempty"`
	Override   bool     `json:"override,omitempty"`
	Stalled    bool     `json:"stalled,omitempty"`
	SeenSpin   bool     `json:"seenSpin"`
}

// Sensor is one temperature.
type Sensor struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Kind   string   `json:"kind"` // cpu, board, nvme, gpu, disk, ssd, group, other
	Temp   *float64 `json:"temp"`
	Asleep bool     `json:"asleep,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// Status is fans-status.json.
type Status struct {
	Updated time.Time   `json:"updated"`
	Version string      `json:"version"`
	Fans    []FanStatus `json:"fans"`
	Sensors []Sensor    `json:"sensors"`
	Error   string      `json:"error,omitempty"`
}

// ---------- Files ----------

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// WriteJSON writes a file atomically (temp file, then rename).
func WriteJSON(path string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadConfig reads fans.json; a missing file is an empty config (every fan
// left to the BIOS).
func LoadConfig(path string) (Config, error) {
	var c Config
	err := readJSON(path, &c)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// LoadStatus reads fans-status.json.
func LoadStatus(path string) (Status, error) {
	var s Status
	err := readJSON(path, &s)
	return s, err
}

// LoadOverride reads fans-override.json (missing = none).
func LoadOverride(path string) *Override {
	var o Override
	if readJSON(path, &o) != nil || o.ID == "" || time.Now().After(o.Until) {
		return nil
	}
	return &o
}
