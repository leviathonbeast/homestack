package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"homestack/internal/fans"
)

// Schedules: the server's cron jobs (alerts, mover, parity), edited from the
// web page. The agent owns /etc/cron.d/homestack-alerts and rewrites it from
// schedules.json; the commands themselves are a fixed list, never taken from
// the browser.

// Schedule says when one task runs.
type Schedule struct {
	ID       string `json:"id"`
	Enabled  bool   `json:"enabled"`
	Every    string `json:"every"`              // minutes, hourly, daily, weekly, monthly
	Interval int    `json:"interval,omitempty"` // minutes: 5, 10, 15, 20 or 30
	Minute   int    `json:"minute"`
	Hour     int    `json:"hour"`
	Weekdays []int  `json:"weekdays,omitempty"` // 0 = Sunday
	Day      int    `json:"day,omitempty"`      // monthly: 1–28
}

type taskDef struct {
	ID, Title, Desc string
	Command         string // script name in ScriptsDir, plus fixed arguments
	Default         Schedule
	AllowMinutes    bool
	NeedsParity     bool
	Action          string // for the last-run result
}

var scheduleTasks = []taskDef{
	{ID: "disk-temp", Title: "Disk temperature check", Desc: "Warns on Discord when a disk runs hot. Sleeping disks aren't woken.",
		Command: "disk-temp-check", AllowMinutes: true,
		Default: Schedule{Enabled: true, Every: "minutes", Interval: 10}},
	{ID: "space", Title: "Disk space check", Desc: "Warns when a disk, pool or the cache is over 90% full, or a pool is unhealthy.",
		Command: "space-check", AllowMinutes: true,
		Default: Schedule{Enabled: true, Every: "hourly", Minute: 17}},
	{ID: "smart", Title: "SMART check", Desc: "Reports new reallocated or pending sectors, cable errors and SSD wear, in plain English.",
		Command: "smart-check",
		Default: Schedule{Enabled: true, Every: "hourly", Minute: 37}},
	{ID: "mover", Title: "Mover", Desc: "Moves files older than the set age from the cache to the array, then posts a summary.",
		Command: "mover", Action: "mover",
		Default: Schedule{Enabled: true, Every: "daily", Hour: 3}},
	{ID: "parity-sync", Title: "Parity sync", Desc: "Updates SnapRAID parity. Skipped if a disk is missing, a pool is unhealthy, or more than 500 files were deleted.",
		Command: "snapraid-job sync", NeedsParity: true, Action: "parity-sync",
		Default: Schedule{Every: "daily", Hour: 4, Minute: 30}},
	{ID: "parity-check", Title: "Parity check", Desc: "Checks part of the array against parity (8% a run), so everything is checked about every three months.",
		Command: "snapraid-job scrub", NeedsParity: true, Action: "parity-check",
		Default: Schedule{Every: "weekly", Hour: 6, Weekdays: []int{0}}},
	{ID: "backup", Title: "Backup", Desc: "Saves Homestack's and the server's settings, encrypted, to the backups folder (Settings → Backups), keeping the newest. Alerts if it fails.",
		Command: "homestack-backup", Action: "backup",
		Default: Schedule{Every: "daily", Hour: 2, Minute: 15}},
}

func taskByID(id string) *taskDef {
	for i := range scheduleTasks {
		if scheduleTasks[i].ID == id {
			return &scheduleTasks[i]
		}
	}
	return nil
}

func (a *Agent) cronFile() string {
	if v := os.Getenv("HOMESTACK_AGENT_CRON"); v != "" {
		return v
	}
	return "/etc/cron.d/homestack-alerts"
}

func (a *Agent) schedulesPath() string { return filepath.Join(a.opt.StateDir, "schedules.json") }

const cronHeader = "# Managed by Homestack (Schedules page). Changes made here are overwritten."

// loadSchedules reads schedules.json, or on first use imports the cron file
// that deploy/debian/notify/install.sh put there, so nothing changes until
// you save.
func (a *Agent) loadSchedules() map[string]Schedule {
	out := map[string]Schedule{}
	if b, err := os.ReadFile(a.schedulesPath()); err == nil {
		var list []Schedule
		if json.Unmarshal(b, &list) == nil {
			for _, s := range list {
				out[s.ID] = s
			}
		}
	} else if b, err := os.ReadFile(a.cronFile()); err == nil {
		maps.Copy(out, importCron(string(b)))
	}
	for _, t := range scheduleTasks {
		if _, ok := out[t.ID]; !ok {
			s := t.Default
			s.ID = t.ID
			if _, err := os.Stat(a.cronFile()); err == nil {
				s.Enabled = false // the file exists but this task isn't in it
			}
			out[t.ID] = s
		}
	}
	return out
}

var reCronLine = regexp.MustCompile(`^(#?)\s*(\S+\s+\S+\s+\S+\s+\S+\s+\S+)\s+root\s+(.+?)\s*$`)

func importCron(text string) map[string]Schedule {
	out := map[string]Schedule{}
	for line := range strings.SplitSeq(text, "\n") {
		m := reCronLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		cmd := strings.Join(strings.Fields(m[3]), " ")
		cmd = strings.TrimPrefix(cmd, "/usr/local/sbin/")
		for _, t := range scheduleTasks {
			if cmd != t.Command {
				continue
			}
			s, ok := parseCron(m[2])
			if !ok {
				s = t.Default
			}
			s.ID, s.Enabled = t.ID, m[1] == ""
			out[t.ID] = s
		}
	}
	return out
}

func atoiIn(s string, lo, hi int) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= lo && n <= hi
}

// parseCron understands the shapes this page writes.
func parseCron(expr string) (Schedule, bool) {
	f := strings.Fields(expr)
	if len(f) != 5 || f[3] != "*" {
		return Schedule{}, false
	}
	if strings.HasPrefix(f[0], "*/") && f[1] == "*" && f[2] == "*" && f[4] == "*" {
		n, ok := atoiIn(f[0][2:], 1, 59)
		return Schedule{Every: "minutes", Interval: n}, ok
	}
	min, ok := atoiIn(f[0], 0, 59)
	if !ok {
		return Schedule{}, false
	}
	if f[1] == "*" && f[2] == "*" && f[4] == "*" {
		return Schedule{Every: "hourly", Minute: min}, true
	}
	hour, ok := atoiIn(f[1], 0, 23)
	if !ok {
		return Schedule{}, false
	}
	switch {
	case f[2] == "*" && f[4] == "*":
		return Schedule{Every: "daily", Minute: min, Hour: hour}, true
	case f[2] == "*":
		var days []int
		for d := range strings.SplitSeq(f[4], ",") {
			n, ok := atoiIn(d, 0, 7)
			if !ok {
				return Schedule{}, false
			}
			days = append(days, n%7)
		}
		return Schedule{Every: "weekly", Minute: min, Hour: hour, Weekdays: days}, true
	case f[4] == "*":
		d, ok := atoiIn(f[2], 1, 28)
		return Schedule{Every: "monthly", Minute: min, Hour: hour, Day: d}, ok
	}
	return Schedule{}, false
}

func (s Schedule) cron() string {
	switch s.Every {
	case "minutes":
		return fmt.Sprintf("*/%d * * * *", s.Interval)
	case "hourly":
		return fmt.Sprintf("%d * * * *", s.Minute)
	case "daily":
		return fmt.Sprintf("%d %d * * *", s.Minute, s.Hour)
	case "weekly":
		d := make([]string, len(s.Weekdays))
		for i, w := range s.Weekdays {
			d[i] = strconv.Itoa(w)
		}
		return fmt.Sprintf("%d %d * * %s", s.Minute, s.Hour, strings.Join(d, ","))
	case "monthly":
		return fmt.Sprintf("%d %d %d * *", s.Minute, s.Hour, s.Day)
	}
	return ""
}

func (s Schedule) matches(t time.Time) bool {
	switch s.Every {
	case "minutes":
		return t.Minute()%s.Interval == 0
	case "hourly":
		return t.Minute() == s.Minute
	}
	if t.Minute() != s.Minute || t.Hour() != s.Hour {
		return false
	}
	switch s.Every {
	case "daily":
		return true
	case "weekly":
		for _, w := range s.Weekdays {
			if int(t.Weekday()) == w {
				return true
			}
		}
		return false
	case "monthly":
		return t.Day() == s.Day
	}
	return false
}

func (s Schedule) next(from time.Time) *time.Time {
	t := from.Truncate(time.Minute).Add(time.Minute)
	for end := t.AddDate(0, 0, 40); t.Before(end); t = t.Add(time.Minute) {
		if s.matches(t) {
			return &t
		}
	}
	return nil
}

func (s *Schedule) validate(t *taskDef) error {
	switch s.Every {
	case "minutes":
		if !t.AllowMinutes {
			return fmt.Errorf("%s: at most once an hour", t.Title)
		}
		switch s.Interval {
		case 5, 10, 15, 20, 30:
		default:
			return fmt.Errorf("%s: every 5, 10, 15, 20 or 30 minutes", t.Title)
		}
		s.Minute, s.Hour, s.Weekdays, s.Day = 0, 0, nil, 0
	case "hourly", "daily", "weekly", "monthly":
		if s.Minute < 0 || s.Minute > 59 || s.Hour < 0 || s.Hour > 23 {
			return fmt.Errorf("%s: pick a valid time", t.Title)
		}
		s.Interval = 0
		if s.Every == "hourly" {
			s.Hour = 0
		}
		if s.Every == "weekly" {
			seen := map[int]bool{}
			var ds []int
			for _, d := range s.Weekdays {
				if d < 0 || d > 6 {
					return fmt.Errorf("%s: unknown day", t.Title)
				}
				if !seen[d] {
					seen[d] = true
					ds = append(ds, d)
				}
			}
			if len(ds) == 0 {
				return fmt.Errorf("%s: pick at least one day", t.Title)
			}
			sort.Ints(ds)
			s.Weekdays = ds
		} else {
			s.Weekdays = nil
		}
		if s.Every == "monthly" {
			if s.Day < 1 || s.Day > 28 {
				return fmt.Errorf("%s: day of the month must be 1–28, so it runs every month", t.Title)
			}
		} else {
			s.Day = 0
		}
	default:
		return fmt.Errorf("%s: unknown frequency", t.Title)
	}
	return nil
}

// TaskView is one row on the Schedules page.
type TaskView struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Desc      string     `json:"desc"`
	Command   string     `json:"command"`
	Schedule  Schedule   `json:"schedule"`
	Default   Schedule   `json:"default"`
	Cron      string     `json:"cron"`
	Next      *time.Time `json:"next,omitempty"`
	Blocked   string     `json:"blocked,omitempty"` // why it can't be switched on
	Minutes   bool       `json:"allowMinutes"`
	LastRun   *JobResult `json:"lastRun,omitempty"`
	Installed bool       `json:"installed"`
}

type schedulesView struct {
	Tasks    []TaskView `json:"tasks"`
	File     string     `json:"file"`
	Managed  bool       `json:"managed"`
	Warnings []string   `json:"warnings"`
	Timezone string     `json:"timezone"`
}

func (a *Agent) blockedReason(t *taskDef) string {
	if _, err := os.Stat(filepath.Join(a.opt.ScriptsDir, strings.Fields(t.Command)[0])); err != nil {
		if t.ID == "backup" {
			return "Not installed yet: run deploy/debian/agent/install.sh again."
		}
		return "Not installed yet: run deploy/debian/notify/install.sh."
	}
	if t.ID == "backup" && a.loadBackupSettings().Key == nil {
		return "Set a backup passphrase first (Settings → Backups)."
	}
	if t.NeedsParity {
		ps := a.Snapshot().Parity
		switch {
		case !ps.Installed:
			return "SnapRAID isn't installed."
		case !ps.Configured:
			return "SnapRAID isn't set up yet (/etc/snapraid.conf)."
		case !ps.Synced:
			return "Run the first parity sync by hand first (sudo snapraid-job sync)."
		}
	}
	return ""
}

func (a *Agent) schedulesView() schedulesView {
	cur := a.loadSchedules()
	now := time.Now()
	v := schedulesView{File: a.cronFile(), Warnings: []string{}}
	v.Timezone, _ = now.Zone()
	if b, err := os.ReadFile(a.cronFile()); err == nil {
		v.Managed = strings.Contains(string(b), cronHeader)
	}
	for i := range scheduleTasks {
		t := &scheduleTasks[i]
		s := cur[t.ID]
		d := t.Default
		d.ID = t.ID
		tv := TaskView{ID: t.ID, Title: t.Title, Desc: t.Desc, Command: t.Command, Schedule: s, Default: d,
			Cron: s.cron(), Minutes: t.AllowMinutes, Blocked: a.blockedReason(t)}
		tv.Installed = !strings.HasPrefix(tv.Blocked, "Not installed")
		if s.Enabled {
			tv.Next = s.next(now)
		}
		if t.Action != "" {
			tv.LastRun = a.jobs.lastResult(t.Action)
		}
		v.Tasks = append(v.Tasks, tv)
	}
	v.Warnings = scheduleWarnings(cur, now)
	return v
}

// scheduleWarnings points out clashes. The shared lock keeps them safe, so
// these are hints, not errors.
func scheduleWarnings(cur map[string]Schedule, now time.Time) []string {
	w := []string{}
	mover, sync, check := cur["mover"], cur["parity-sync"], cur["parity-check"]
	if !mover.Enabled {
		w = append(w, "The mover is off, so new files stay on the cache until you run it from the Storage page.")
	}
	if sync.Enabled && mover.Enabled {
		ms, ss := mover.next(now), sync.next(now)
		if ms != nil && ss != nil && ss.Sub(*ms) >= 0 && ss.Sub(*ms) < time.Hour && mover.Every == "daily" && sync.Every == "daily" {
			w = append(w, "Parity sync starts less than an hour after the mover. If the mover is still going, the sync waits for it, but only for an hour; after that it skips the night. Leave a bigger gap if the mover often runs long.")
		}
		if ms != nil && ss != nil && ss.Before(*ms) && ms.Sub(*ss) < 12*time.Hour && mover.Every == "daily" && sync.Every == "daily" {
			w = append(w, "Parity sync runs before the mover, so files moved tonight aren't protected until tomorrow night's sync. Put the sync after the mover.")
		}
	}
	if check.Enabled && !sync.Enabled {
		w = append(w, "Parity check is on but parity sync is off. Checks compare against old parity and will report every change since then as an error.")
	}
	return w
}

func (a *Agent) writeCron(cur map[string]Schedule) error {
	var b strings.Builder
	b.WriteString(cronHeader + "\n# Settings: " + a.schedulesPath() + "\nSHELL=/bin/bash\nPATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n\n")
	for _, t := range scheduleTasks {
		s := cur[t.ID]
		fmt.Fprintf(&b, "# %s\n", t.Title)
		prefix := ""
		if !s.Enabled {
			prefix = "#"
		}
		fmt.Fprintf(&b, "%s%-16s root  %s\n\n", prefix, s.cron(), t.Command)
	}
	// cron ignores files with a dot in the name, so the temp file is safe.
	tmp := filepath.Join(filepath.Dir(a.cronFile()), "."+filepath.Base(a.cronFile())+".new")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o644)
	return os.Rename(tmp, a.cronFile())
}

func (a *Agent) putSchedules(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedules []Schedule `json:"schedules"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	cur := a.loadSchedules()
	for _, s := range req.Schedules {
		t := taskByID(s.ID)
		if t == nil {
			writeErr(w, 400, "unknown task "+s.ID)
			return
		}
		if err := s.validate(t); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if s.Enabled && !cur[s.ID].Enabled {
			if why := a.blockedReason(t); why != "" {
				writeErr(w, 400, t.Title+": "+why)
				return
			}
		}
		cur[s.ID] = s
	}
	list := make([]Schedule, 0, len(cur))
	for _, t := range scheduleTasks {
		list = append(list, cur[t.ID])
	}
	if err := fans.WriteJSON(a.schedulesPath(), list, 0o640); err != nil {
		writeErr(w, 500, "couldn't save the settings")
		return
	}
	if err := a.writeCron(cur); err != nil {
		writeErr(w, 500, "couldn't write "+a.cronFile()+": "+err.Error())
		return
	}
	writeJSON(w, 200, a.schedulesView())
}
