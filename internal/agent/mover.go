package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"homestack/internal/fans"
)

// Mover settings from the web page: how old a file must be before the mover
// moves it. Saved in mover.json and written, with the share list, to the
// overrides file the mover reads after /etc/default/homestack-alerts
// (/etc/default/homestack-mover-shares).

type MoverSettings struct {
	AgeMinutes int `json:"ageMinutes"` // 0 = not set here: the alerts file decides
}

const maxMoverAge = 30 * 24 * 60

func (a *Agent) moverSettingsPath() string { return filepath.Join(a.opt.StateDir, "mover.json") }

func (a *Agent) loadMoverSettings() MoverSettings {
	var m MoverSettings
	if b, err := os.ReadFile(a.moverSettingsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// alertsMoverAge is MOVER_AGE_MIN from the alerts settings (1440 if unset).
func (a *Agent) alertsMoverAge() int {
	b, err := os.ReadFile(a.alertsConf())
	if err != nil {
		return 1440
	}
	if m := regexp.MustCompile(`(?m)^\s*MOVER_AGE_MIN=["']?(\d+)`).FindSubmatch(b); m != nil {
		if n, err := strconv.Atoi(string(m[1])); err == nil {
			return n
		}
	}
	return 1440
}

func (a *Agent) moverView() map[string]any {
	set := a.loadMoverSettings()
	age := set.AgeMinutes
	if age == 0 {
		age = a.alertsMoverAge()
	}
	return map[string]any{"ageMinutes": age, "defaultMinutes": a.alertsMoverAge(), "custom": set.AgeMinutes > 0}
}

func (a *Agent) putMover(w http.ResponseWriter, r *http.Request) {
	var req MoverSettings
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if req.AgeMinutes < 0 || req.AgeMinutes > maxMoverAge {
		writeErr(w, 400, "Pick an age between 1 minute and 30 days.")
		return
	}
	if err := fans.WriteJSON(a.moverSettingsPath(), req, 0o640); err != nil {
		writeErr(w, 500, "Couldn't save the mover settings.")
		return
	}
	var err error
	if st, ok := a.loadShares(); ok {
		err = a.writeMoverFile(&st)
	} else {
		err = a.writeMoverFile(nil)
	}
	if err != nil {
		writeErr(w, 500, "Saved, but the mover's settings file couldn't be written: "+err.Error())
		return
	}
	writeJSON(w, 200, a.moverView())
}

// writeMoverFile writes the mover's overrides: the share list when shares are
// managed here (st != nil), and the age when it was set on the web page.
func (a *Agent) writeMoverFile(st *sharesState) error {
	var b strings.Builder
	b.WriteString("# Written by Homestack (Shares page and Settings → Storage). Read by the mover.\n")
	if st != nil {
		b.WriteString("MOVER_SHARES=\"" + strings.Join(a.moverShareList(*st), " ") + "\"\n")
	}
	if age := a.loadMoverSettings().AgeMinutes; age > 0 {
		fmt.Fprintf(&b, "MOVER_AGE_MIN=%d\n", age)
	}
	tmp := a.moverSharesFile() + ".new"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.moverSharesFile())
}
