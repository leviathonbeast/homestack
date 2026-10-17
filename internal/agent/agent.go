// Package agent is Homestack's host service (`homestack agent`). It runs on
// the server itself (not in Docker) because it needs the hardware and the
// storage: fan control, pool/disk/parity status, and a short, fixed list of
// storage jobs with safety checks. Homestack talks to it over a unix socket.
//
// The agent keeps its own state on the boot disk (/var/lib/homestack-agent),
// so fans and array controls keep working even when the array is stopped.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options configure the agent. Paths are overridable for tests.
type Options struct {
	Socket     string // /run/homestack/agent.sock
	StateDir   string // /var/lib/homestack-agent
	LogDir     string // /var/log/homestack-agent
	SysRoot    string // "" = real /sys
	DataDir    string // Homestack's data folder (for Discord settings), may be unavailable
	Version    string
	Runner     Runner
	ScriptsDir string // /usr/local/sbin
	LockFile   string // /run/lock/homestack-array.lock
	NetDir     string // /etc/systemd/network
	Source     string // Homestack's source folder, where install.sh ran from ("" = unknown)
	Interval   time.Duration
}

// Runner runs external commands (swapped for fakes in tests).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.CombinedOutput()
}

// Agent is the running service.
type Agent struct {
	opt Options
	run Runner

	mu     sync.RWMutex
	status Status

	disks *diskPoller
	jobs  *jobManager
	fans  *fanController
	net   *netManager
	arc   *arcManager

	xattr xattrOps    // mergerfs control files (faked in tests)
	lay   layoutCache // where the storage is mounted (layout.go)
	io    *ioTracker  // per-disk reads and writes (io.go)

	started time.Time
}

// Status is everything the Storage and Fans pages show.
type Status struct {
	Updated time.Time       `json:"updated"`
	Version string          `json:"version"`
	Host    string          `json:"host"`
	Array   ArrayStatus     `json:"array"`
	Pools   []Pool          `json:"pools"`
	Cache   *Filesystem     `json:"cache,omitempty"`
	Disks   []Disk          `json:"disks"`
	Parity  ParityStatus    `json:"parity"`
	Mover   *JobResult      `json:"mover,omitempty"`
	Jobs    []Job           `json:"jobs"`
	Running *Job            `json:"running,omitempty"`
	Fans    []FanView       `json:"fans"`
	Sensors []SensorView    `json:"sensors"`
	Warn    []Warning       `json:"warnings"`
	Totals  map[string]any  `json:"totals"`
	UPS     *UPS            `json:"ups,omitempty"`
	Slots   []ArraySlot     `json:"slots"`             // the data disks in snapraid.conf
	Replace *ReplaceSummary `json:"replace,omitempty"` // a disk replacement in progress
	Hold    string          `json:"hold,omitempty"`    // why the mover and parity jobs are paused
	Layout  Layout          `json:"layout"`            // where the storage is mounted
	Source  string          `json:"source,omitempty"`  // Homestack's source folder the agent was installed from
}

// Warning is a banner on the Storage page.
type Warning struct {
	Level string `json:"level"` // warning | error
	Text  string `json:"text"`
}

// New creates an agent with defaults filled in.
func New(opt Options) *Agent {
	if opt.Socket == "" {
		opt.Socket = "/run/homestack/agent.sock"
	}
	if opt.StateDir == "" {
		opt.StateDir = "/var/lib/homestack-agent"
	}
	if opt.LogDir == "" {
		opt.LogDir = "/var/log/homestack-agent"
	}
	if opt.ScriptsDir == "" {
		opt.ScriptsDir = "/usr/local/sbin"
	}
	if opt.LockFile == "" {
		opt.LockFile = "/run/lock/homestack-array.lock"
	}
	if opt.NetDir == "" {
		opt.NetDir = "/etc/systemd/network"
	}
	if opt.Interval == 0 {
		opt.Interval = 5 * time.Second
	}
	if opt.Runner == nil {
		opt.Runner = execRunner{}
	}
	a := &Agent{opt: opt, run: opt.Runner, started: time.Now(), xattr: realXattr}
	a.disks = newDiskPoller(a)
	a.jobs = newJobManager(a)
	a.fans = newFanController(a)
	a.net = newNetManager(a)
	a.arc = newARCManager(a)
	a.io = newIOTracker()
	return a
}

func (a *Agent) sys(p string) string { return filepath.Join(a.opt.SysRoot, p) }

func (a *Agent) cmd(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := a.run.Run(c, name, args...)
	return string(out), err
}

// Run starts everything and serves the socket until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	for _, d := range []string{a.opt.StateDir, a.opt.LogDir, filepath.Dir(a.opt.Socket)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	a.jobs.load()
	if err := a.writeLayoutFile(a.layout()); err != nil {
		log.Printf("couldn't write %s: %v", a.layoutFile(), err)
	}
	// A network change nobody kept before the agent went away is undone now.
	a.net.recover(ctx, "the host agent restarted before it was kept")
	go a.disks.loop(ctx)
	go a.fans.loop(ctx)
	go a.statusLoop(ctx)
	go a.resumeReplace(ctx, 90*time.Second)
	go a.spinLoop(ctx)

	_ = os.Remove(a.opt.Socket)
	ln, err := net.Listen("unix", a.opt.Socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(a.opt.Socket, 0o660)
	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("homestack agent %s listening on %s", a.opt.Version, a.opt.Socket)
	err = srv.Serve(ln)
	a.jobs.shutdown(75 * time.Second)
	a.net.recover(context.Background(), "the host agent stopped before it was kept")
	a.fans.restoreAll()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *Agent) statusLoop(ctx context.Context) {
	a.refresh(ctx)
	t := time.NewTicker(a.opt.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refresh(ctx)
		}
	}
}

// refresh gathers a fresh status.
func (a *Agent) refresh(ctx context.Context) {
	st := Status{Updated: time.Now(), Version: a.opt.Version}
	st.Host, _ = os.Hostname()
	st.Pools = a.pools(ctx)
	st.Cache = a.cacheFS(ctx)
	st.Disks = a.disks.snapshot()
	a.io.sample(a.sys("/proc/diskstats"), st.Updated)
	a.io.fill(st.Disks, st.Updated)
	st.Array = a.arrayStatus(ctx, st.Pools)
	st.Parity = a.parityStatus(ctx)
	st.Jobs, st.Running = a.jobs.snapshot()
	st.Mover = a.jobs.lastResult("mover")
	st.Fans, st.Sensors = a.fans.view()
	st.Totals = totals(st.Pools)
	st.UPS = a.upsStatus(ctx)
	st.Slots = a.slots(ctx, st.Pools, st.Disks, st.Parity, false)
	a.rememberDisks(st.Pools, st.Disks, st.Slots)
	st.Replace = a.replaceSummary()
	st.Hold = a.hold()
	st.Layout, st.Source = a.layout(), a.opt.Source
	st.Warn = warnings(st)
	a.mu.Lock()
	a.status = st
	a.mu.Unlock()
}

// Snapshot returns the latest status.
func (a *Agent) Snapshot() Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}

func totals(pools []Pool) map[string]any {
	var size, alloc, free uint64
	n := 0
	for _, p := range pools {
		if !p.Array {
			continue
		}
		size += p.Size
		alloc += p.Alloc
		free += p.Free
		n++
	}
	return map[string]any{"disks": n, "size": size, "used": alloc, "free": free}
}

func warnings(st Status) []Warning {
	w := []Warning{}
	for _, p := range st.Pools {
		if p.Health != "ONLINE" {
			w = append(w, Warning{"error", fmt.Sprintf("Pool %s is %s.", p.Name, p.Health)})
		} else if p.Errors != "" && !strings.Contains(p.Errors, "No known data errors") {
			w = append(w, Warning{"error", fmt.Sprintf("Pool %s reports: %s", p.Name, p.Errors)})
		}
		if p.Cap >= 90 {
			w = append(w, Warning{"warning", fmt.Sprintf("Pool %s is %d%% full.", p.Name, p.Cap)})
		}
		if !p.Mounted && p.Health == "ONLINE" && st.Array.State == "started" {
			w = append(w, Warning{"error", fmt.Sprintf("Pool %s isn't mounted.", p.Name)})
		}
	}
	for _, d := range st.Disks {
		if d.Health == "failed" {
			w = append(w, Warning{"error", fmt.Sprintf("%s says it is failing (SMART).", d.Label)})
		}
	}
	if st.Parity.Configured && st.Parity.DaysSinceSync >= 3 {
		w = append(w, Warning{"warning", fmt.Sprintf("Parity was last synced %d days ago.", st.Parity.DaysSinceSync)})
	}
	if u := st.UPS; u != nil {
		switch {
		case u.Error != "":
			w = append(w, Warning{"warning", u.Error + " A power cut wouldn't shut the server down cleanly."})
		case u.OnBattery:
			w = append(w, Warning{"error", "Running on UPS battery: the power is off."})
		}
		for _, f := range u.Flags {
			if f == "RB" {
				w = append(w, Warning{"warning", "The UPS says its battery needs replacing."})
			}
		}
	}
	for _, sl := range st.Slots {
		if sl.State == "MISSING" {
			w = append(w, Warning{"error", fmt.Sprintf("%s is missing: its pool isn't imported. Check its cable and power, or replace it (Storage → Replace disk).", sl.Pool)})
		}
	}
	if st.Hold != "" {
		w = append(w, Warning{"warning", "The mover and parity jobs are paused: " + st.Hold})
	}
	if st.Parity.Errors != "" {
		w = append(w, Warning{"error", st.Parity.Errors})
	}
	return w
}

// ---------- API ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *Agent) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		st := a.Snapshot()
		st.Jobs, st.Running = a.jobs.snapshot() // always current, for progress bars
		writeJSON(w, 200, st)
	})
	mux.HandleFunc("POST /v1/actions", a.postAction)
	mux.HandleFunc("GET /v1/jobs/{id}/log", a.jobLog)
	mux.HandleFunc("GET /v1/runs", a.getRuns)
	mux.HandleFunc("GET /v1/runs/log", a.getRunLog)
	mux.HandleFunc("GET /v1/disks/smart", a.getSMART)
	mux.HandleFunc("GET /v1/mover", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, a.moverView())
	})
	mux.HandleFunc("PUT /v1/mover", a.putMover)
	mux.HandleFunc("GET /v1/fans/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, a.fans.configView())
	})
	mux.HandleFunc("PUT /v1/fans/config", a.fans.putConfig)
	mux.HandleFunc("POST /v1/fans/test", a.fans.postTest)
	mux.HandleFunc("GET /v1/isos", a.listISOs)
	mux.HandleFunc("GET /v1/schedules", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, a.schedulesView())
	})
	mux.HandleFunc("PUT /v1/schedules", a.putSchedules)
	mux.HandleFunc("GET /v1/shares", a.handleShares)
	mux.HandleFunc("POST /v1/shares", a.postShares)
	mux.HandleFunc("GET /v1/files", a.listFiles)
	mux.HandleFunc("GET /v1/files/download", a.downloadFile)
	mux.HandleFunc("GET /v1/files/content", a.readFileContent)
	mux.HandleFunc("PUT /v1/files/content", a.writeFileContent)
	mux.HandleFunc("POST /v1/files", a.fileOp)
	mux.HandleFunc("POST /v1/files/plan", a.planFiles)
	mux.HandleFunc("GET /v1/zfs/arc", a.arc.getARC)
	mux.HandleFunc("PUT /v1/zfs/arc", a.arc.putARC)
	mux.HandleFunc("GET /v1/replace", a.getReplace)
	mux.HandleFunc("POST /v1/replace", a.postReplace)
	mux.HandleFunc("GET /v1/array", a.getArray)
	mux.HandleFunc("POST /v1/array", a.postArray)
	mux.HandleFunc("GET /v1/spindown", a.getSpin)
	mux.HandleFunc("PUT /v1/spindown", a.putSpin)
	mux.HandleFunc("GET /v1/parity", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.parityView()) })
	mux.HandleFunc("PUT /v1/parity", a.putParity)
	mux.HandleFunc("GET /v1/storage/layout", a.getLayout)
	mux.HandleFunc("PUT /v1/storage/layout", a.putLayout)
	mux.HandleFunc("GET /v1/backups", a.getBackups)
	mux.HandleFunc("PUT /v1/backups/settings", a.putBackupSettings)
	mux.HandleFunc("POST /v1/backups", a.postBackup)
	mux.HandleFunc("GET /v1/backups/file", a.getBackupFile)
	mux.HandleFunc("POST /v1/backups/restore", a.postRestore)
	mux.HandleFunc("GET /v1/net", a.net.getView)
	mux.HandleFunc("GET /v1/net/file", a.net.getFile)
	mux.HandleFunc("POST /v1/net/plan", a.net.postPlan)
	mux.HandleFunc("POST /v1/net/apply", a.net.postApply)
	mux.HandleFunc("POST /v1/net/confirm", a.net.postConfirm)
	mux.HandleFunc("POST /v1/net/cancel", a.net.postCancel)
	return mux
}

func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// listISOs lets Homestack list installer ISOs without mounting the array.
func (a *Agent) listISOs(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" || !filepath.IsAbs(dir) || strings.Contains(dir, "..") {
		writeErr(w, 400, "bad folder")
		return
	}
	type iso struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	out := []iso{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.Count(strings.TrimPrefix(p, dir), "/") > 2 {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".iso") {
			if fi, err := d.Info(); err == nil {
				out = append(out, iso{d.Name(), p, fi.Size()})
			}
		}
		return nil
	})
	writeJSON(w, 200, out)
}
