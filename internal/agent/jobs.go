package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Job is one run of a storage task.
type Job struct {
	ID       string     `json:"id"`
	Action   string     `json:"action"`
	Title    string     `json:"title"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	State    string     `json:"state"` // running, ok, failed, stopped
	Detail   string     `json:"detail,omitempty"`
	Progress *float64   `json:"progress,omitempty"`
	ETA      string     `json:"eta,omitempty"`
	ETASecs  *int64     `json:"etaSeconds,omitempty"`
	Done     *uint64    `json:"doneBytes,omitempty"`  // data processed so far
	Total    *uint64    `json:"totalBytes,omitempty"` // when it's known (a copy, a resilver)
	Speed    string     `json:"speed,omitempty"`
	Step     string     `json:"step,omitempty"`
	Paused   *time.Time `json:"paused,omitempty"`   // since when (Pause)
	ResumeAt *time.Time `json:"resumeAt,omitempty"` // when a timed pause ends
	// PausedSecs is how long it has been paused in all.
	PausedSecs int64 `json:"pausedSecs,omitempty"`
}

// JobResult is the last outcome of a kind of job (including cron runs).
type JobResult struct {
	Finished time.Time `json:"finished"`
	OK       bool      `json:"ok"`
	Summary  string    `json:"summary"`
}

type jobManager struct {
	a       *Agent
	mu      sync.Mutex
	history []Job
	running *Job
	proc    *exec.Cmd
	cancel  context.CancelFunc
	// extraLog is the output of the command an in-agent job is running
	// right now (shown with the job's own log).
	extraLog string
}

func newJobManager(a *Agent) *jobManager { return &jobManager{a: a} }

func (m *jobManager) statePath(name string) string { return filepath.Join(m.a.opt.StateDir, name) }

func (m *jobManager) load() {
	b, err := os.ReadFile(m.statePath("jobs.json"))
	if err == nil {
		_ = json.Unmarshal(b, &m.history)
	}
	// A job that was running when the agent stopped didn't finish.
	for i := range m.history {
		if m.history[i].State == "running" {
			m.history[i].State = "stopped"
			m.history[i].Detail = "The agent was restarted while this was running."
		}
	}
}

func (m *jobManager) saveLocked() {
	b, _ := json.MarshalIndent(m.history, "", "  ")
	tmp := m.statePath("jobs.json.tmp")
	if os.WriteFile(tmp, b, 0o640) == nil {
		_ = os.Rename(tmp, m.statePath("jobs.json"))
	}
}

func (m *jobManager) snapshot() ([]Job, *Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := append([]Job(nil), m.history...)
	if h == nil {
		h = []Job{}
	}
	if m.running != nil {
		r := *m.running
		m.updateProgress(&r)
		if r.Paused != nil { // the log's last speed and ETA are from before
			r.Speed, r.ETA, r.ETASecs = "", "", nil
		}
		return h, &r
	}
	return h, nil
}

// lastResult merges agent history with results the scripts record for cron
// runs (/var/lib/homestack-agent/last-<action>.json).
func (m *jobManager) lastResult(action string) *JobResult {
	var best *JobResult
	if b, err := os.ReadFile(m.statePath("last-" + action + ".json")); err == nil {
		var r JobResult
		if json.Unmarshal(b, &r) == nil {
			best = &r
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.history {
		if j.Action == action && j.Finished != nil && (best == nil || j.Finished.After(best.Finished)) {
			best = &JobResult{Finished: *j.Finished, OK: j.State == "ok", Summary: j.Detail}
		}
	}
	return best
}

// recordResult saves how a run outside the job list ended (snapraid-job's
// record_result does the same). The last one that worked is kept apart, so
// a failed or skipped run doesn't hide when parity was last complete.
func (m *jobManager) recordResult(action string, r JobResult) {
	b, _ := json.Marshal(r)
	b = append(b, '\n')
	_ = os.WriteFile(m.statePath("last-"+action+".json"), b, 0o644)
	if r.OK {
		_ = os.WriteFile(m.statePath("last-ok-"+action+".json"), b, 0o644)
	}
}

// lastOK is the most recent successful run of an action.
func (m *jobManager) lastOK(action string) *JobResult {
	var best *JobResult
	for _, name := range []string{"last-ok-" + action + ".json", "last-" + action + ".json"} {
		b, err := os.ReadFile(m.statePath(name))
		var r JobResult
		if err == nil && json.Unmarshal(b, &r) == nil && r.OK && (best == nil || r.Finished.After(best.Finished)) {
			best = &r
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.history {
		if j.Action == action && j.Finished != nil && j.State == "ok" && (best == nil || j.Finished.After(best.Finished)) {
			best = &JobResult{Finished: *j.Finished, OK: true, Summary: j.Detail}
		}
	}
	return best
}

func (m *jobManager) runningAction() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		return ""
	}
	return m.running.Action
}

// ---------- The array lock (shared with the mover, snapraid-job and flatten-dataset) ----------

func (m *jobManager) tryLock() (*os.File, error) {
	f, err := os.OpenFile(m.a.opt.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("busy")
	}
	return f, nil
}

func unlock(f *os.File) {
	if f != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

// lockHolder says what (if anything) is using the array right now.
func (m *jobManager) lockHolder() string {
	m.mu.Lock()
	if m.running != nil {
		t := m.running.Title
		m.mu.Unlock()
		return t
	}
	m.mu.Unlock()
	f, err := m.tryLock()
	if err != nil {
		if b, err := os.ReadFile(m.a.opt.LockFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b))
		}
		return "another storage task"
	}
	unlock(f)
	return ""
}

// ---------- Actions ----------

// ActionRequest is what Homestack sends.
type ActionRequest struct {
	Action  string `json:"action"`
	Pool    string `json:"pool,omitempty"`
	Confirm string `json:"confirm,omitempty"`
	Force   bool   `json:"force,omitempty"`   // parity sync after mass deletion / force VMs off
	Disk    string `json:"disk,omitempty"`    // disk-spindown: sdc
	Minutes int    `json:"minutes,omitempty"` // job-pause: carry on after this long (0: on Resume)
	Path    string `json:"path,omitempty"`    // fix-permissions
	// file-copy, file-move
	Paths    []string `json:"paths,omitempty"`
	Dest     string   `json:"dest,omitempty"`
	Conflict string   `json:"conflict,omitempty"`
}

var confirmWords = map[string]string{"array-stop": "STOP", "reboot": "REBOOT", "shutdown": "SHUTDOWN"}

type refusal struct{ msg string }

func (r refusal) Error() string { return r.msg }

func refuse(format string, a ...any) error { return refusal{fmt.Sprintf(format, a...)} }

func (a *Agent) postAction(w http.ResponseWriter, r *http.Request) {
	var req ActionRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if want, ok := confirmWords[req.Action]; ok && req.Confirm != want {
		writeErr(w, 400, fmt.Sprintf("Type %s to confirm.", want))
		return
	}
	job, err := a.startAction(r.Context(), req)
	if err != nil {
		if rf, ok := errors.AsType[refusal](err); ok {
			writeErr(w, 409, rf.msg)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	a.refresh(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true, "job": job})
}

func (a *Agent) poolByName(ctx context.Context, name string) (*Pool, error) {
	for _, p := range a.pools(ctx) {
		if p.Name == name {
			p := p
			return &p, nil
		}
	}
	return nil, refuse("There's no pool called %q.", name)
}

func (a *Agent) startAction(ctx context.Context, req ActionRequest) (*Job, error) {
	m := a.jobs
	switch req.Action {
	case "pool-scrub", "pool-scrub-pause", "pool-scrub-stop":
		p, err := a.poolByName(ctx, req.Pool)
		if err != nil {
			return nil, err
		}
		args := map[string][]string{"pool-scrub": {"scrub"}, "pool-scrub-pause": {"scrub", "-p"}, "pool-scrub-stop": {"scrub", "-s"}}[req.Action]
		if req.Action == "pool-scrub" && p.ScanState == "resilvering" {
			return nil, refuse("%s is rebuilding (resilvering). Wait for that to finish.", p.Name)
		}
		if out, err := a.cmd(ctx, 30*time.Second, "zpool", append(args, p.Name)...); err != nil {
			return nil, refuse("zpool: %s", strings.TrimSpace(out))
		}
		return nil, nil
	case "job-stop":
		return nil, m.stop()
	case "job-pause":
		if req.Minutes < 0 || req.Minutes > maxPause {
			return nil, refuse("Pause for up to a week.")
		}
		return nil, m.pause(time.Duration(req.Minutes) * time.Minute)
	case "job-resume":
		return nil, m.resume()
	case "disk-spindown":
		for _, d := range a.disks.snapshot() {
			if d.Name != req.Disk {
				continue
			}
			if !spinEligible(d) {
				return nil, refuse("Only SATA hard disks are spun down (not SSDs, NVMe or the boot drive).")
			}
			if !a.have(ctx, "hdparm") {
				return nil, refuse("hdparm isn't installed. Install it: sudo apt install hdparm")
			}
			if err := a.spinDown(ctx, d.Name); err != nil {
				return nil, refuse("%s", err.Error())
			}
			return nil, nil
		}
		return nil, refuse("There's no disk called %q.", req.Disk)
	}

	// Everything else is a job that needs the array to itself.
	if t := m.pausedTitle(); t != "" {
		return nil, refuse("Busy: %s is paused. Resume it, or stop it, first.", t)
	}
	if h := m.lockHolder(); h != "" {
		return nil, refuse("Busy: %s is running. Wait for it to finish, or stop it first.", h)
	}
	// SnapRAID started by hand in a terminal doesn't take the lock.
	if a.snapraidRunning(ctx) {
		return nil, refuse("Busy: SnapRAID is running in a terminal (outside Homestack). Wait for it to finish.")
	}
	switch req.Action {
	case "parity-sync", "parity-check":
		if err := a.parityPreflight(ctx, req.Action); err != nil {
			return nil, err
		}
		sub := map[string]string{"parity-sync": "sync", "parity-check": "scrub"}[req.Action]
		args := []string{sub}
		if req.Force && req.Action == "parity-sync" {
			args = append(args, "--force")
		}
		title := map[string]string{"parity-sync": "Parity sync", "parity-check": "Parity check"}[req.Action]
		return m.runCommand(req.Action, title, filepath.Join(a.opt.ScriptsDir, "snapraid-job"), args...)
	case "fix-permissions":
		p, _, err := a.resolve(ctx, req.Path, true)
		if err != nil {
			return nil, refuse("%s", err.Error())
		}
		if fi, err := os.Stat(a.sys(p)); err != nil || !fi.IsDir() {
			return nil, refuse("Pick a folder.")
		}
		return m.runFunc("fix-permissions", "New permissions: "+p, func(ctx context.Context, log func(string)) error {
			return a.fixPermissions(ctx, p, log)
		})
	case "file-copy", "file-move":
		op := strings.TrimPrefix(req.Action, "file-")
		p, err := a.planTransfer(ctx, TransferRequest{Op: op, Paths: req.Paths, Dest: req.Dest, Conflict: req.Conflict})
		if err != nil {
			return nil, err
		}
		title := fmt.Sprintf("%s %d item%s to %s", map[string]string{"copy": "Copy", "move": "Move"}[op], p.Items, plural(p.Items), p.Dest)
		return m.runFunc(req.Action, title, func(ctx context.Context, log func(string)) error {
			return a.runTransfer(ctx, p, req.Conflict == "replace", log)
		})
	case "mover":
		if h := a.hold(); h != "" {
			return nil, refuse("The mover is paused: %s", h)
		}
		if st := a.arrayStatus(ctx, a.pools(ctx)); st.State != "started" {
			return nil, refuse("The array isn't started, so there's nowhere to move files to.")
		}
		return m.runCommand("mover", "Mover", filepath.Join(a.opt.ScriptsDir, "mover"))
	case "trim":
		pools := a.pools(ctx)
		return m.runFunc("trim", "TRIM SSDs", func(ctx context.Context, log func(string)) error {
			sum, err := a.trim(ctx, pools, log)
			if err == nil {
				log(sum)
				m.setProgress(func(j *Job) { j.Detail = sum })
			}
			return err
		})
	case "array-stop":
		if err := a.stopPreflight(ctx, false); err != nil {
			return nil, err
		}
		return m.runFunc("array-stop", "Stopping the array", func(ctx context.Context, log func(string)) error {
			return a.arrayStop(ctx, log, req.Force)
		})
	case "array-start":
		if err := a.startPreflight(ctx); err != nil {
			return nil, err
		}
		return m.runFunc("array-start", "Starting the array", func(ctx context.Context, log func(string)) error {
			return a.arrayStart(ctx, log)
		})
	case "reboot", "shutdown":
		if err := a.stopPreflight(ctx, true); err != nil {
			return nil, err
		}
		verb := map[string]string{"reboot": "Rebooting", "shutdown": "Shutting down"}[req.Action]
		return m.runFunc(req.Action, verb, func(ctx context.Context, log func(string)) error {
			return a.powerOff(ctx, log, req.Action)
		})
	}
	return nil, refuse("Unknown action.")
}

// parityPreflight refuses to touch parity when the array isn't whole:
// a missing disk looks like "every file deleted" to SnapRAID.
func (a *Agent) parityPreflight(ctx context.Context, action string) error {
	if h := a.hold(); h != "" {
		return refuse("Parity jobs are paused: %s Finish that first (Storage → Replace disk).", h)
	}
	ps := a.parityStatus(ctx)
	if !ps.Installed {
		return refuse("SnapRAID isn't installed.")
	}
	if !ps.Configured {
		return refuse("SnapRAID isn't set up yet (%s is missing).", a.snapraidConf())
	}
	for _, d := range ps.Data {
		if !d.Mounted {
			return refuse("Data disk %s (%s) isn't mounted. Parity is left untouched until every disk is back.", d.Name, d.Path)
		}
	}
	for _, d := range ps.Parity {
		if !d.Mounted {
			return refuse("The parity disk (%s) isn't mounted.", d.Path)
		}
	}
	for _, p := range a.pools(ctx) {
		if p.Health != "ONLINE" {
			return refuse("Pool %s is %s. Fix that before touching parity.", p.Name, p.Health)
		}
		if p.Errors != "" && !strings.Contains(p.Errors, "No known data errors") {
			return refuse("Pool %s has errors (%s). Fix those before touching parity.", p.Name, p.Errors)
		}
		if p.ScanState == "resilvering" {
			return refuse("Pool %s is rebuilding. Wait for that to finish.", p.Name)
		}
	}
	for _, d := range a.disks.snapshot() {
		if d.Health == "failed" && (strings.HasPrefix(d.Role, "disk") || d.Role == "parity") {
			return refuse("%s is failing SMART. Replace it (using parity) before syncing.", d.Label)
		}
	}
	if action == "parity-check" && !ps.Synced {
		return refuse("There's no parity to check yet. Run the first sync.")
	}
	return nil
}

func (a *Agent) stopPreflight(ctx context.Context, power bool) error {
	for _, p := range a.pools(ctx) {
		if p.ScanState == "resilvering" {
			return refuse("Pool %s is rebuilding (resilvering). Don't stop or restart until it finishes.", p.Name)
		}
		if p.ScanState == "scrubbing" && !power {
			return refuse("Pool %s is being scrubbed. Pause it first (it resumes where it left off).", p.Name)
		}
	}
	return nil
}

func (a *Agent) startPreflight(ctx context.Context) error {
	pools := a.pools(ctx)
	if len(pools) == 0 && len(a.plainDataDisks(ctx)) == 0 {
		return refuse("No ZFS pools are imported, and snapraid.conf has no other data disks in fstab. Check the disks are connected.")
	}
	for _, p := range pools {
		if p.Health != "ONLINE" {
			return refuse("Pool %s is %s. Check it with `zpool status %s` before starting.", p.Name, p.Health, p.Name)
		}
	}
	return nil
}

// ---------- Running jobs ----------

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *jobManager) logPath(id string) string {
	return filepath.Join(m.a.opt.LogDir, "job-"+id+".log")
}

func (m *jobManager) begin(action, title string) (*Job, *os.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running != nil {
		return nil, nil, refuse("Busy: %s is running.", m.running.Title)
	}
	j := &Job{ID: newID(), Action: action, Title: title, Started: time.Now(), State: "running"}
	f, err := os.OpenFile(m.logPath(j.ID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, nil, err
	}
	m.running = j
	return j, f, nil
}

func (m *jobManager) finish(j *Job, err error, stopped bool) {
	m.mu.Lock()
	now := time.Now()
	j.Finished = &now
	switch {
	case stopped:
		j.State, j.Detail = "stopped", "Stopped by you."
		if v, _ := stopRequested.Load(j.ID); v == "agent" {
			j.Detail = "Stopped cleanly because the agent was restarting. Run it again to carry on."
		}
	case err != nil:
		j.State = "failed"
		if j.Detail == "" {
			j.Detail = err.Error()
		}
	default:
		j.State = "ok"
	}
	if j.Paused != nil {
		j.PausedSecs += int64(now.Sub(*j.Paused).Seconds())
	}
	if j.PausedSecs >= 60 && j.Detail != "" {
		j.Detail += fmt.Sprintf(" Paused for %s in all.", humanDur(time.Duration(j.PausedSecs)*time.Second))
	}
	j.Progress, j.ETA, j.Speed, j.Paused, j.ResumeAt = nil, "", "", nil, nil
	m.history = append([]Job{*j}, m.history...)
	if len(m.history) > 50 {
		for _, old := range m.history[50:] {
			_ = os.Remove(m.logPath(old.ID))
		}
		m.history = m.history[:50]
	}
	m.running, m.proc, m.cancel = nil, nil, nil
	m.saveLocked()
	m.mu.Unlock()
}

// runCommand runs one of the allow-listed scripts in the background.
func (m *jobManager) runCommand(action, title, name string, args ...string) (*Job, error) {
	j, logf, err := m.begin(action, title)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LC_ALL=C", "HOMESTACK_AGENT_JOB="+j.ID, "RUN_LOGS="+m.a.runsDir())
	if err := cmd.Start(); err != nil {
		logf.Close()
		m.finish(j, err, false)
		return nil, err
	}
	m.mu.Lock()
	m.proc = cmd
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		logf.Close()
		stopped := cmd.ProcessState != nil && !cmd.ProcessState.Success() && m.wasStopped(j.ID)
		detail := lastLines(m.logPath(j.ID), 3)
		if s := m.a.scriptSummary(j.ID); s != "" { // the script's own words for how it ended
			detail = s
		}
		m.setProgress(func(j *Job) { j.Detail = detail })
		m.finish(j, err, stopped)
		m.a.finishRunLog(j)
		m.a.refresh(context.Background())
	}()
	return j, nil
}

var stopRequested sync.Map

func (m *jobManager) wasStopped(id string) bool { _, ok := stopRequested.Load(id); return ok }

// stopWanted says whether Stop (or an agent shutdown) was asked for the
// running job.
func (m *jobManager) stopWanted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		return false
	}
	_, ok := stopRequested.Load(m.running.ID)
	return ok
}

// stopByAgent says the running job is being stopped because the agent is
// shutting down (not by you).
func (m *jobManager) stopByAgent() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		return false
	}
	v, _ := stopRequested.Load(m.running.ID)
	return v == "agent"
}

// setProgress updates the running job's progress fields.
func (m *jobManager) setProgress(f func(j *Job)) {
	m.mu.Lock()
	if m.running != nil {
		f(m.running)
	}
	m.mu.Unlock()
}

// runLogged runs a command inside an in-agent job (runFunc), with its output
// in logPath and SnapRAID's progress lines fed to the job. It's registered as
// the job's process, so Stop and an agent shutdown send it SIGINT (SnapRAID
// then stops cleanly) instead of it being killed. rc is the exit code.
func (m *jobManager) runLogged(ctx context.Context, logPath, name string, args ...string) (int, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return -1, err
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if err := cmd.Start(); err != nil {
		f.Close()
		return -1, err
	}
	m.mu.Lock()
	m.proc, m.extraLog = cmd, logPath
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var werr error
wait:
	for {
		select {
		case werr = <-done:
			break wait
		case <-t.C:
			if mm := reSnapProgress.FindStringSubmatch(lastProgressLine(logPath)); mm != nil {
				m.setProgress(func(j *Job) { applySnapProgress(j, mm) })
			}
		}
	}
	f.Close()
	m.mu.Lock()
	m.proc, m.extraLog = nil, ""
	if m.running != nil {
		m.running.Progress, m.running.ETA, m.running.ETASecs, m.running.Speed, m.running.Done, m.running.Total = nil, "", nil, "", nil, nil
	}
	m.mu.Unlock()
	rc := 0
	if cmd.ProcessState != nil {
		rc = cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if werr != nil && !errors.As(werr, &ee) {
		return rc, werr
	}
	return rc, nil
}

// runFunc runs an in-agent sequence (array stop/start, power) as a job.
func (m *jobManager) runFunc(action, title string, fn func(ctx context.Context, log func(string)) error) (*Job, error) {
	j, logf, err := m.begin(action, title)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	go func() {
		lock, lerr := m.tryLock()
		if lerr == nil {
			_ = lock.Truncate(0)
			_, _ = lock.WriteString(title)
		}
		logFn := func(s string) {
			fmt.Fprintf(logf, "%s %s\n", time.Now().Format("15:04:05"), s)
			m.mu.Lock()
			if m.running != nil {
				m.running.Step = s
			}
			m.mu.Unlock()
		}
		var err error
		if lerr != nil {
			err = errors.New("another storage task started first")
		} else {
			err = fn(ctx, logFn)
		}
		if err != nil {
			how := "Failed: "
			if m.wasStopped(j.ID) {
				how = "Stopped: "
			}
			logFn(how + err.Error())
			j.Detail = err.Error()
		} else if j.Detail == "" { // the job may have set its own summary
			j.Detail = "Done."
		}
		unlock(lock)
		logf.Close()
		m.finish(j, err, err != nil && m.wasStopped(j.ID))
		m.a.refresh(context.Background())
	}()
	return j, nil
}

// stop asks the running job to stop cleanly (SIGINT: SnapRAID saves its
// progress and can resume).
func (m *jobManager) stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		return refuse("Nothing is running.")
	}
	switch m.running.Action {
	case "array-stop", "array-start", "reboot", "shutdown":
		return refuse("%s can't be interrupted halfway; it will finish on its own.", m.running.Title)
	}
	stopRequested.Store(m.running.ID, true)
	if m.proc != nil && m.proc.Process != nil {
		_ = syscall.Kill(-m.proc.Process.Pid, syscall.SIGINT)
	}
	m.continueLocked() // a paused job only sees the SIGINT once it runs again
	if m.cancel != nil {
		m.cancel()
	}
	return nil
}

// ---------- Pause ----------

// Parity jobs can be paused: their processes are stopped (SIGSTOP) right
// where they are, and carry on from there (SIGCONT) on Resume, like
// Unraid's parity check. Meanwhile the array lock stays held, so the mover
// and the scheduled parity jobs don't run; a timed pause carries on by
// itself, so a forgotten one can't hold them up for long.
var pausable = map[string]bool{"parity-sync": true, "parity-check": true}

const maxPause = 7 * 24 * 60 // minutes

func (m *jobManager) pause(after time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.running
	switch {
	case j == nil:
		return refuse("Nothing is running.")
	case !pausable[j.Action]:
		return refuse("%s can't be paused. Stop it instead.", j.Title)
	case m.proc == nil || m.proc.Process == nil:
		return refuse("%s hasn't started yet.", j.Title)
	}
	if _, ok := stopRequested.Load(j.ID); ok {
		return refuse("%s is stopping.", j.Title)
	}
	if err := syscall.Kill(-m.proc.Process.Pid, syscall.SIGSTOP); err != nil {
		return fmt.Errorf("couldn't pause it: %v", err)
	}
	now := time.Now()
	if j.Paused == nil { // pausing again only changes when it carries on
		j.Paused = &now
	}
	j.ResumeAt = nil
	if after > 0 {
		at := now.Add(after)
		j.ResumeAt = &at
		id := j.ID
		time.AfterFunc(after, func() { m.autoResume(id, at) })
	}
	return nil
}

func (m *jobManager) resume() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil || m.running.Paused == nil {
		return refuse("Nothing is paused.")
	}
	m.continueLocked()
	return nil
}

// autoResume ends a timed pause, unless it was resumed or paused again since.
func (m *jobManager) autoResume(id string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.running; j != nil && j.ID == id && j.ResumeAt != nil && j.ResumeAt.Equal(at) {
		m.continueLocked()
	}
}

// continueLocked lets a paused job run again (m.mu held).
func (m *jobManager) continueLocked() {
	j := m.running
	if j == nil || j.Paused == nil {
		return
	}
	if m.proc != nil && m.proc.Process != nil {
		_ = syscall.Kill(-m.proc.Process.Pid, syscall.SIGCONT)
	}
	j.PausedSecs += int64(time.Since(*j.Paused).Seconds())
	j.Paused, j.ResumeAt = nil, nil
}

// pausedTitle is the paused job's title, if one is paused.
func (m *jobManager) pausedTitle() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running != nil && m.running.Paused != nil {
		return m.running.Title
	}
	return ""
}

// shutdown runs when the agent itself is stopped (update, restart, host
// shutdown): a running parity job gets SIGINT, like the Stop button, so
// SnapRAID saves its progress instead of being killed halfway.
func (m *jobManager) shutdown(wait time.Duration) {
	m.mu.Lock()
	if m.running == nil || m.proc == nil || m.proc.Process == nil {
		m.mu.Unlock()
		return
	}
	id, pid := m.running.ID, m.proc.Process.Pid
	stopRequested.Store(id, "agent")
	_ = syscall.Kill(-pid, syscall.SIGINT)
	m.continueLocked()
	m.mu.Unlock()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		done := m.running == nil || m.running.ID != id
		m.mu.Unlock()
		if done {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// byteProgress shows how far a copy has got: done of total bytes, with the
// speed counted from (since, from). total 0 clears it.
func (m *jobManager) byteProgress(done, total uint64, since time.Time, from uint64) {
	m.setProgress(func(j *Job) {
		if total == 0 {
			j.Progress, j.Done, j.Total, j.Speed, j.ETA, j.ETASecs = nil, nil, nil, "", "", nil
			return
		}
		pct := math.Round(float64(done)*1000/float64(total)) / 10
		j.Progress, j.Done, j.Total = &pct, &done, &total
		if secs := time.Since(since).Seconds(); secs >= 5 && done > from {
			rate := float64(done-from) / secs
			eta := int64(float64(total-done) / rate)
			j.Speed, j.ETASecs = human(uint64(rate))+"/s", &eta
		}
	})
}

// ---------- Progress from logs ----------

// SnapRAID prints lines like "42%, 1234567 MB, 245 MB/s, 1024 stripe/s, CPU 9%, 3:21 ETA".
var reSnapProgress = regexp.MustCompile(`(\d+)%, (\d+) MB(?:, (\d+) MB/s)?.*?(?:(\d+):(\d+) ETA)?\s*$`)

func (m *jobManager) updateProgress(j *Job) {
	var logs []string
	switch j.Action {
	case "parity-sync":
		logs = []string{"/var/log/snapraid-sync.log"}
	case "parity-check":
		logs = []string{"/var/log/snapraid-scrub.log"}
	default:
		return
	}
	for _, l := range logs {
		last := lastProgressLine(l)
		if mm := reSnapProgress.FindStringSubmatch(last); mm != nil {
			applySnapProgress(j, mm)
		}
	}
}

// applySnapProgress fills a job from one SnapRAID progress line:
// "42%, 1234567 MB, 245 MB/s, 1024 stripe/s, CPU 9%, 3:21 ETA".
// The percentage (and SnapRAID's ETA) counts block positions; the MB is
// what's been read at them, which depends on how many disks have data at
// each position (all of them near the start, fewer past the end of the
// emptier disks). So no total is guessed from the two: it would drift.
func applySnapProgress(j *Job, mm []string) {
	v, _ := strconv.ParseFloat(mm[1], 64)
	j.Progress = &v
	if mb, err := strconv.ParseUint(mm[2], 10, 64); err == nil {
		done := mb << 20
		j.Done, j.Total = &done, nil
	}
	if mm[3] != "" {
		j.Speed = mm[3] + " MB/s"
	}
	if mm[4] != "" {
		h, _ := strconv.ParseInt(mm[4], 10, 64)
		m, _ := strconv.ParseInt(mm[5], 10, 64)
		secs := h*3600 + m*60
		j.ETASecs = &secs
		j.ETA = mm[4] + ":" + mm[5]
	}
}

func lastProgressLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, _ := f.Stat()
	off := max(fi.Size()-4096, 0)
	_, _ = f.Seek(off, io.SeekStart)
	b, _ := io.ReadAll(f)
	// Progress may be written with carriage returns.
	parts := strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' || r == '\r' })
	for _, part := range slices.Backward(parts) {
		if strings.Contains(part, "%,") {
			return part
		}
	}
	return ""
}

func lastLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if t != "" {
			lines = append(lines, t)
			if len(lines) > n {
				lines = lines[1:]
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (a *Agent) jobLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 12 || strings.Trim(id, "0123456789abcdef") != "" {
		writeErr(w, 400, "bad id")
		return
	}
	b, err := os.ReadFile(a.jobs.logPath(id))
	if err != nil {
		writeErr(w, 404, "no log")
		return
	}
	// Parity jobs and the mover write their own run logs (see runs.go). Show
	// the one this job wrote, but only when the job itself printed next to
	// nothing (the mover prints its log itself, so it isn't repeated).
	// Jobs from before run logs existed fall back to the shared log, which
	// only still belongs to the latest run of its kind.
	a.jobs.mu.Lock()
	extra := ""
	if a.jobs.running != nil && a.jobs.running.ID == id {
		extra = a.jobs.extraLog
	}
	a.jobs.mu.Unlock()
	quiet := len(strings.Fields(string(b))) < 12
	if extra != "" {
		if t := tailSnapLog(extra, 200); t != "" {
			b = append(b, []byte("\n----- now running ("+filepath.Base(extra)+") -----\n"+t)...)
		}
	} else if run := a.runLogForJob(id); run != "" {
		if t, _ := readRunLog(run, 300); t != "" && quiet {
			b = append(b, []byte("\n----- "+filepath.Base(run)+" -----\n"+t)...)
		}
	} else if action, latest := a.jobs.jobInfo(id); latest && quiet {
		if path := snapLogFor(action); path != "" {
			if extra := tailSnapLog(path, 300); extra != "" {
				b = append(b, []byte("\n----- "+path+" (last lines) -----\n"+extra)...)
			}
		}
	}
	if len(b) > 256<<10 {
		b = b[len(b)-256<<10:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

// jobInfo says what a job was and whether it's the running or the most
// recent job of that action.
func (m *jobManager) jobInfo(id string) (action string, latest bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running != nil && m.running.ID == id {
		return m.running.Action, true
	}
	for _, j := range m.history { // newest first
		if j.ID == id {
			action = j.Action
			break
		}
	}
	if action == "" || (m.running != nil && m.running.Action == action) {
		return action, false
	}
	for _, j := range m.history {
		if j.Action == action {
			return action, j.ID == id
		}
	}
	return action, false
}

func snapLogFor(action string) string {
	switch action {
	case "parity-sync":
		return "/var/log/snapraid-sync.log"
	case "parity-check":
		return "/var/log/snapraid-scrub.log"
	case "mover":
		return "/var/log/homestack-mover.log"
	}
	return ""
}

// tailSnapLog returns the last lines of a SnapRAID log, with runs of progress
// lines ("42%, 1234 MB, ...") collapsed to the latest one.
func tailSnapLog(path string, maxLines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(fi.Size()-512<<10, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) == 0 {
		return ""
	}
	return collapseProgress(string(buf), maxLines, off > 0)
}

// collapseProgress keeps the last maxLines lines of a log, with runs of
// SnapRAID progress lines collapsed to the latest one. cut says the text
// starts partway through the log, so its first line may be incomplete.
func collapseProgress(text string, maxLines int, cut bool) string {
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []string
	for l := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if reSnapProgress.MatchString(l) && len(out) > 0 && reSnapProgress.MatchString(out[len(out)-1]) {
			out[len(out)-1] = l
			continue
		}
		out = append(out, l)
	}
	if cut && len(out) > 0 {
		out = out[1:] // first line may be cut
	}
	if len(out) > maxLines {
		out = out[len(out)-maxLines:]
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// ---------- Small persistent markers ----------

func (a *Agent) writeTime(name string) {
	_ = os.WriteFile(filepath.Join(a.opt.StateDir, name), []byte(time.Now().Format(time.RFC3339)), 0o640)
}

func (a *Agent) readTime(name string) *time.Time {
	b, err := os.ReadFile(filepath.Join(a.opt.StateDir, name))
	if err != nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return nil
	}
	return &t
}
