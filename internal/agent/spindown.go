package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"homestack/internal/fans"
)

// Spin-down, like Unraid's spin down delay: a hard disk that has done
// nothing (no reads or writes in /proc/diskstats) for the delay is put in
// standby with hdparm -y. Off unless a delay is set (Settings → Storage).
// Only SATA hard disks: never SSDs, NVMe, USB or the boot drive. Nothing
// is spun down while a storage job runs (unless it's paused), and checking
// a disk never wakes it: hdparm -C and SMART's standby check aren't reads
// or writes.

type SpinSettings struct {
	DelayMinutes int `json:"delayMinutes"` // 0 = never
}

// The delays offered (minutes), as on Unraid.
var spinDelays = []int{0, 15, 30, 45, 60, 90, 120, 180, 240, 300, 360, 480, 600, 720}

func (a *Agent) spinPath() string { return filepath.Join(a.opt.StateDir, "spindown.json") }

func (a *Agent) loadSpin() SpinSettings {
	var s SpinSettings
	if b, err := os.ReadFile(a.spinPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// spinEligible says whether a disk may be spun down.
func spinEligible(d Disk) bool {
	return d.Rotational && d.Transport == "sata" && d.Role != "system"
}

// asleep asks the disk whether it's in standby, without waking it.
func (a *Agent) asleep(ctx context.Context, name string) bool {
	out, _ := a.cmd(ctx, 15*time.Second, "hdparm", "-C", "/dev/"+name)
	return strings.Contains(out, "standby") || strings.Contains(out, "sleeping")
}

// spinDown puts a disk in standby now.
func (a *Agent) spinDown(ctx context.Context, name string) error {
	if out, err := a.cmd(ctx, 30*time.Second, "hdparm", "-y", "/dev/"+name); err != nil {
		return fmt.Errorf("hdparm: %s", strings.TrimSpace(out))
	}
	a.disks.setAsleep(name, true)
	return nil
}

// spinLoop checks once a minute for disks that have been idle long enough.
func (a *Agent) spinLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.spinTick(ctx, time.Now())
		}
	}
}

func (a *Agent) spinTick(ctx context.Context, now time.Time) {
	delay := time.Duration(a.loadSpin().DelayMinutes) * time.Minute
	// A paused parity job leaves the disks idle: they may sleep meanwhile.
	busy := a.jobs.lockHolder() != "" && a.jobs.pausedTitle() == ""
	if delay <= 0 || busy || a.opNow().active() || !a.have(ctx, "hdparm") {
		return
	}
	for _, d := range a.disks.snapshot() {
		if !spinEligible(d) || d.Asleep || a.io.idleFor(d.Name, now) < delay {
			continue
		}
		if a.asleep(ctx, d.Name) {
			a.disks.setAsleep(d.Name, true)
			continue
		}
		if err := a.spinDown(ctx, d.Name); err != nil {
			log.Printf("couldn't spin down %s: %v", d.Name, err)
			continue
		}
		log.Printf("spun down %s (%s) after %s without reads or writes", d.Name, d.Label, delay)
	}
}

// SpinView is the spin-down setting.
type SpinView struct {
	DelayMinutes int    `json:"delayMinutes"`
	Delays       []int  `json:"delays"`
	Disks        int    `json:"disks"`             // how many disks it applies to
	Problem      string `json:"problem,omitempty"` // why it can't work
}

func (a *Agent) spinView(ctx context.Context) SpinView {
	v := SpinView{DelayMinutes: a.loadSpin().DelayMinutes, Delays: spinDelays}
	for _, d := range a.disks.snapshot() {
		if spinEligible(d) {
			v.Disks++
		}
	}
	if !a.have(ctx, "hdparm") {
		v.Problem = "hdparm isn't installed. Install it: sudo apt install hdparm (the agent installer does)."
	}
	return v
}

func (a *Agent) getSpin(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.spinView(r.Context()))
}

func (a *Agent) putSpin(w http.ResponseWriter, r *http.Request) {
	var s SpinSettings
	if err := readBody(r, &s); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	ok := false
	for _, d := range spinDelays {
		ok = ok || d == s.DelayMinutes
	}
	if !ok {
		writeErr(w, 400, "Pick one of the delays offered.")
		return
	}
	if err := fans.WriteJSON(a.spinPath(), s, 0o640); err != nil {
		writeErr(w, 500, "Couldn't save it: "+err.Error())
		return
	}
	writeJSON(w, 200, a.spinView(r.Context()))
}
