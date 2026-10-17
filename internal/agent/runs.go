package agent

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Run logs: the mover and the parity jobs keep one log per run, whether cron
// or Homestack started it, so a new run never wipes the last one. The scripts
// (deploy/debian/notify/mover and snapraid-job) write them to <LogDir>/runs:
//
//	<action>-<YYYYmmdd-HHMMSS>[-<job id>].log   the latest run
//	<action>-<...>.log.gz                       older runs, compressed
//	<action>-<...>.json                         how it ended
//
// They keep the newest LOG_KEEP runs of each kind (30 by default), and the
// old fixed paths (/var/log/snapraid-sync.log…) link to the latest run.

// RunLog is one run of the mover or a parity job.
type RunLog struct {
	Name    string    `json:"name"`
	Action  string    `json:"action"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"` // last write: when it finished, once it has
	Size    int64     `json:"size"`
	JobID   string    `json:"jobId,omitempty"` // started from Homestack
	Running bool      `json:"running,omitempty"`
	OK      *bool     `json:"ok,omitempty"` // nil: it didn't record how it ended
	Summary string    `json:"summary,omitempty"`
}

var reRunName = regexp.MustCompile(`^(parity-sync|parity-check|mover)-(\d{8}-\d{6})(?:-([0-9a-f]{12}))?\.log(\.gz)?$`)

func (a *Agent) runsDir() string { return filepath.Join(a.opt.LogDir, "runs") }

// runSidecar is the .json next to a run log.
func runSidecar(logPath string) string {
	return strings.TrimSuffix(strings.TrimSuffix(logPath, ".gz"), ".log") + ".json"
}

// listRuns returns the runs of one action ("" = all), newest first.
func (a *Agent) listRuns(action string) []RunLog {
	des, err := os.ReadDir(a.runsDir())
	if err != nil {
		return []RunLog{}
	}
	running := a.jobs.runningAction()
	if running == "" {
		if h := a.jobs.lockHolder(); h != "" {
			running = map[string]string{"Mover": "mover", "Parity sync": "parity-sync", "Parity check": "parity-check"}[h]
		}
	}
	out := []RunLog{}
	for _, de := range des {
		m := reRunName.FindStringSubmatch(de.Name())
		if m == nil || (action != "" && m[1] != action) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		started, err := time.ParseInLocation("20060102-150405", m[2], time.Local)
		if err != nil {
			continue
		}
		r := RunLog{Name: de.Name(), Action: m[1], Started: started, Updated: info.ModTime(), Size: info.Size(), JobID: m[3]}
		if b, err := os.ReadFile(runSidecar(filepath.Join(a.runsDir(), de.Name()))); err == nil {
			var res JobResult
			if json.Unmarshal(b, &res) == nil {
				ok := res.OK
				r.OK, r.Summary = &ok, res.Summary
				if !res.Finished.IsZero() {
					r.Updated = res.Finished
				}
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	// Only the newest run of a kind can still be going.
	seen := map[string]bool{}
	for i := range out {
		if !seen[out[i].Action] {
			seen[out[i].Action] = true
			out[i].Running = out[i].OK == nil && out[i].Action == running
		}
	}
	return out
}

// runLogForJob finds the run log a job started from Homestack wrote.
func (a *Agent) runLogForJob(id string) string {
	for _, ext := range []string{".log", ".log.gz"} {
		if m, _ := filepath.Glob(filepath.Join(a.runsDir(), "*-"+id+ext)); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// finishRunLog records how a job ended when its script couldn't (stopped,
// killed), so the run doesn't show as unfinished forever.
// scriptSummary is how a script run by job id said it ended (snapraid-job
// and the mover write it next to their run log), or "".
func (a *Agent) scriptSummary(id string) string {
	p := a.runLogForJob(id)
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(runSidecar(p))
	var r JobResult
	if err != nil || json.Unmarshal(b, &r) != nil {
		return ""
	}
	return r.Summary
}

func (a *Agent) finishRunLog(j *Job) {
	p := a.runLogForJob(j.ID)
	if p == "" || j.Finished == nil {
		return
	}
	if _, err := os.Stat(runSidecar(p)); err == nil {
		return
	}
	b, _ := json.Marshal(JobResult{Finished: *j.Finished, OK: j.State == "ok", Summary: j.Detail})
	_ = os.WriteFile(runSidecar(p), append(b, '\n'), 0o640)
}

// readRunLog returns the end of a run log (compressed or not), with runs of
// SnapRAID progress lines collapsed to the latest one.
func readRunLog(path string, maxLines int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return "", err
		}
		defer gz.Close()
		r = io.LimitReader(gz, 1<<30)
	}
	buf, cut := tailBytes(r, 512<<10)
	return collapseProgress(string(buf), maxLines, cut), nil
}

// tailBytes keeps the last n bytes of r. cut says whether anything before
// them was dropped.
func tailBytes(r io.Reader, n int) ([]byte, bool) {
	buf := make([]byte, 0, 2*n)
	chunk := make([]byte, 64<<10)
	cut := false
	for {
		k, err := r.Read(chunk)
		buf = append(buf, chunk[:k]...)
		if len(buf) > 2*n {
			buf = buf[:copy(buf, buf[len(buf)-n:])]
			cut = true
		}
		if err != nil {
			break
		}
	}
	if len(buf) > n {
		buf = buf[len(buf)-n:]
		cut = true
	}
	return buf, cut
}

func (a *Agent) getRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.listRuns(r.URL.Query().Get("action")))
}

func (a *Agent) getRunLog(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !reRunName.MatchString(name) {
		writeErr(w, 400, "bad name")
		return
	}
	text, err := readRunLog(filepath.Join(a.runsDir(), name), 3000)
	if err != nil {
		writeErr(w, 404, "That log is gone (only the newest runs are kept).")
		return
	}
	if text == "" {
		text = "(nothing was written to this log)\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprint(w, text)
}
