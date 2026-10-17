package server

import (
	"context"
	"log"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"
)

// updateChecker compares each image a container uses with the registry's
// current version. It asks the Docker daemon (GET /distribution/...), which
// handles registry logins, and only fetches the small manifest, not the image.
type updateChecker struct {
	s       *Server
	mu      sync.Mutex
	results map[string]updateResult // image ref -> result
	lastRun time.Time
	running bool
	wakeCh  chan struct{}
}

type updateResult struct {
	Image     string    `json:"image"`
	Available bool      `json:"available"`
	Checked   time.Time `json:"checked"`
	Error     string    `json:"error,omitempty"`
}

func newUpdateChecker(s *Server) *updateChecker {
	return &updateChecker{s: s, results: map[string]updateResult{}, wakeCh: make(chan struct{}, 1)}
}

func normalizeRef(ref string) string {
	if strings.Contains(ref, "@") {
		return ref
	}
	slash := strings.LastIndex(ref, "/")
	if strings.LastIndex(ref, ":") <= slash {
		ref += ":latest"
	}
	return ref
}

func (u *updateChecker) available(ref string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.results[normalizeRef(ref)].Available
}

func (u *updateChecker) wake() {
	select {
	case u.wakeCh <- struct{}{}:
	default:
	}
}

func (u *updateChecker) loop(ctx context.Context) {
	// Give the system a moment after boot before the first check.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	for {
		hours := u.s.store.Settings().UpdateCheckHours
		if hours > 0 {
			u.mu.Lock()
			due := time.Since(u.lastRun) >= time.Duration(hours)*time.Hour
			u.mu.Unlock()
			if due {
				u.checkAll(ctx)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-u.wakeCh:
		case <-time.After(5 * time.Minute):
		}
	}
}

// imageRefs returns the image references used by any container.
func (u *updateChecker) imageRefs(ctx context.Context, onlyStack string) []string {
	cs, err := u.s.docker.Containers(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var refs []string
	if onlyStack != "" {
		stacks, err := u.s.buildStacks(ctx)
		if err != nil {
			return nil
		}
		for _, st := range stacks {
			if st.Name == onlyStack {
				for _, c := range st.Containers {
					if !seen[c.Image] {
						seen[c.Image] = true
						refs = append(refs, c.Image)
					}
				}
			}
		}
		return refs
	}
	for _, c := range cs {
		// Skip containers started from an image ID or digest.
		if strings.HasPrefix(c.Image, "sha256:") || strings.Contains(c.Image, "@") {
			continue
		}
		if !seen[c.Image] {
			seen[c.Image] = true
			refs = append(refs, c.Image)
		}
	}
	return refs
}

// begin marks a full check as running; false if one already is.
func (u *updateChecker) begin() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.running {
		return false
	}
	u.running = true
	return true
}

func (u *updateChecker) checkAll(ctx context.Context) {
	if !u.begin() {
		return
	}
	u.runAll(ctx)
}

func (u *updateChecker) runAll(ctx context.Context) {
	defer func() {
		u.mu.Lock()
		u.running = false
		u.lastRun = time.Now()
		u.mu.Unlock()
	}()
	refs := u.imageRefs(ctx, "")
	u.check(ctx, refs, true)
	n := 0
	u.mu.Lock()
	snap := make(map[string]updateResult, len(u.results))
	for k, r := range u.results {
		snap[k] = r
		if r.Available {
			n++
		}
	}
	u.mu.Unlock()
	if ctx.Err() == nil {
		u.s.notifyNewUpdates(snap)
	}
	log.Printf("update check: %d images checked, %d with updates", len(refs), n)
}

func (u *updateChecker) check(ctx context.Context, refs []string, replaceAll bool) {
	fresh := map[string]updateResult{}
	for _, ref := range refs {
		if ctx.Err() != nil {
			return
		}
		fresh[normalizeRef(ref)] = u.checkOne(ctx, ref)
		time.Sleep(250 * time.Millisecond) // be gentle with registry rate limits
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if replaceAll {
		u.results = fresh
		return
	}
	maps.Copy(u.results, fresh)
}

func (u *updateChecker) checkOne(ctx context.Context, ref string) updateResult {
	res := updateResult{Image: ref, Checked: time.Now()}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	local, err := u.s.docker.ImageRepoDigests(cctx, ref)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if len(local) == 0 {
		res.Error = "built locally, nothing to compare"
		return res
	}
	remote, err := u.s.docker.RemoteDigest(cctx, ref)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for _, d := range local {
		if strings.HasSuffix(d, "@"+remote) {
			return res
		}
	}
	res.Available = true
	return res
}

// recheckStack refreshes results for one stack's images (after an update).
func (u *updateChecker) recheckStack(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	u.check(ctx, u.imageRefs(ctx, name), false)
}

func (u *updateChecker) recheckRefs(refs []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	u.check(ctx, refs, false)
}

func (u *updateChecker) snapshot() (list []updateResult, last time.Time, running bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, r := range u.results {
		list = append(list, r)
	}
	if list == nil {
		list = []updateResult{}
	}
	return list, u.lastRun, u.running
}

// ---------- Handlers ----------

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request) {
	list, last, running := s.updates.snapshot()
	var lastRun any
	if !last.IsZero() {
		lastRun = last
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":  list,
		"lastRun":  lastRun,
		"running":  running,
		"interval": s.store.Settings().UpdateCheckHours,
	})
}

func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updates.begin() {
		go s.updates.runAll(context.Background())
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}
