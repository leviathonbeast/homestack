package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"homestack/internal/fans"
)

// Parity job settings from the web page (Settings → Storage): how much of
// the array each parity check reads, how old a block must be before it's
// checked again, and the sync safety limits. Saved in parity.json and
// written to the overrides file snapraid-job reads after the alerts
// settings (/etc/default/homestack-parity). One not set here comes from
// the alerts settings, or SnapRAID's defaults.

type paritySetting struct {
	Key      string `json:"key"` // the variable snapraid-job reads
	Default  int    `json:"default"`
	Min, Max int
}

var paritySettings = []paritySetting{
	{"SNAPRAID_SCRUB_PERCENT", 8, 1, 100},
	{"SNAPRAID_SCRUB_AGE", 10, 0, 3650},
	{"SNAPRAID_DELETE_LIMIT", 500, 0, 100_000_000},
	{"SNAPRAID_UPDATE_LIMIT", 1000, 0, 100_000_000},
}

// ParitySettings maps a setting's key to the value set on the web page.
type ParitySettings map[string]int

func (a *Agent) parityPath() string { return filepath.Join(a.opt.StateDir, "parity.json") }
func (a *Agent) parityFile() string {
	return filepath.Join(filepath.Dir(a.alertsConf()), "homestack-parity")
}

func (a *Agent) loadParity() ParitySettings {
	s := ParitySettings{}
	if b, err := os.ReadFile(a.parityPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// parityValue is a setting as snapraid-job sees it: set here, else in the
// alerts settings, else the default.
func (a *Agent) parityValue(key string) int {
	for _, p := range paritySettings {
		if p.Key == key {
			if v, ok := a.loadParity()[key]; ok {
				return v
			}
			return a.alertsInt(key, p.Default)
		}
	}
	return 0
}

// writeParityFile writes the overrides for snapraid-job (only what's set).
func (a *Agent) writeParityFile(s ParitySettings) error {
	var b strings.Builder
	b.WriteString("# Written by Homestack (Settings → Storage → Parity). snapraid-job reads it after /etc/default/homestack-alerts.\n")
	for _, p := range paritySettings {
		if v, ok := s[p.Key]; ok {
			fmt.Fprintf(&b, "%s=%d\n", p.Key, v)
		}
	}
	tmp := a.parityFile() + ".new"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.parityFile())
}

// ParityView is the Parity settings card: each setting's value now and
// the one it falls back to.
type ParityView struct {
	Values   map[string]int `json:"values"`
	Defaults map[string]int `json:"defaults"` // without the web page's settings
	Custom   bool           `json:"custom"`
}

func (a *Agent) parityView() ParityView {
	s := a.loadParity()
	v := ParityView{Values: map[string]int{}, Defaults: map[string]int{}, Custom: len(s) > 0}
	for _, p := range paritySettings {
		v.Defaults[p.Key] = a.alertsInt(p.Key, p.Default)
		v.Values[p.Key] = a.parityValue(p.Key)
	}
	return v
}

func (a *Agent) putParity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Values ParitySettings `json:"values"` // empty: back to the alerts settings
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	s := ParitySettings{}
	for _, p := range paritySettings {
		v, ok := req.Values[p.Key]
		if !ok {
			continue
		}
		if v < p.Min || v > p.Max {
			writeErr(w, 400, fmt.Sprintf("%s has to be between %d and %d.", paritySettingName(p.Key), p.Min, p.Max))
			return
		}
		s[p.Key] = v
	}
	if err := fans.WriteJSON(a.parityPath(), s, 0o640); err != nil {
		writeErr(w, 500, "Couldn't save it: "+err.Error())
		return
	}
	if err := a.writeParityFile(s); err != nil {
		writeErr(w, 500, "Saved, but snapraid-job's settings file couldn't be written: "+err.Error())
		return
	}
	writeJSON(w, 200, a.parityView())
}

func paritySettingName(key string) string {
	return map[string]string{
		"SNAPRAID_SCRUB_PERCENT": "How much each check covers (%)",
		"SNAPRAID_SCRUB_AGE":     "How old a block must be (days)",
		"SNAPRAID_DELETE_LIMIT":  "The deleted files limit",
		"SNAPRAID_UPDATE_LIMIT":  "The modified files limit",
	}[key]
}
