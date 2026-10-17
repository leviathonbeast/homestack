package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"homestack/internal/docker"
)

// A stack is a folder in the stacks directory containing a compose file.
// Containers are linked to it through the labels `docker compose` adds.

type PortView struct {
	Public  int    `json:"public"`
	Private int    `json:"private"`
	Type    string `json:"type"`
}

type ContainerView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Image           string     `json:"image"`
	State           string     `json:"state"`
	Status          string     `json:"status"`
	Health          string     `json:"health,omitempty"`
	Created         int64      `json:"created"`
	Ports           []PortView `json:"ports"`
	WebUI           string     `json:"webUI,omitempty"`      // saved link, or a detected one
	WebUISaved      bool       `json:"webUISaved,omitempty"` // true when the admin set it
	Stack           string     `json:"stack,omitempty"`
	Service         string     `json:"service,omitempty"`
	Networks        []string   `json:"networks"`
	UpdateAvailable bool       `json:"updateAvailable"`
	CPU             *float64   `json:"cpu,omitempty"` // percent of one core, while a page is watching
	Mem             *uint64    `json:"mem,omitempty"` // bytes
}

type StackView struct {
	Name            string          `json:"name"`
	Dir             string          `json:"dir,omitempty"`
	ComposeFile     string          `json:"composeFile,omitempty"`
	Status          string          `json:"status"` // running, partial, stopped, inactive
	Running         int             `json:"running"`
	Total           int             `json:"total"`
	External        bool            `json:"external"`
	UpdateAvailable bool            `json:"updateAvailable"`
	Busy            string          `json:"busy,omitempty"`
	Modified        int64           `json:"modified,omitempty"`
	CPU             *float64        `json:"cpu,omitempty"` // its running containers added up
	Mem             *uint64         `json:"mem,omitempty"`
	Containers      []ContainerView `json:"containers"`
}

var projectNameStrip = regexp.MustCompile(`[^a-z0-9_-]`)

// composeProjectName mirrors how compose names a project after its folder.
func composeProjectName(dir string) string {
	return projectNameStrip.ReplaceAllString(strings.ToLower(filepath.Base(dir)), "")
}

func (s *Server) containerView(c docker.Container) ContainerView {
	v := ContainerView{
		ID:      c.ID,
		Name:    c.Name(),
		Image:   c.Image,
		State:   c.State,
		Status:  c.Status,
		Created: c.Created,
		Service: c.Labels[docker.LabelService],
		Stack:   c.Labels[docker.LabelProject],
		Ports:   []PortView{},
	}
	if l := s.appLinkFor(v.Name); l != nil {
		v.WebUI, v.WebUISaved = l.URL, true
	} else {
		v.WebUI = webURL(c)
	}
	switch {
	case strings.Contains(c.Status, "(healthy)"):
		v.Health = "healthy"
	case strings.Contains(c.Status, "(unhealthy)"):
		v.Health = "unhealthy"
	case strings.Contains(c.Status, "(health: starting)"):
		v.Health = "starting"
	}
	seen := map[string]bool{}
	for _, p := range c.Ports {
		key := fmt.Sprintf("%d/%d/%s", p.PublicPort, p.PrivatePort, p.Type)
		if seen[key] { // IPv4 and IPv6 bindings of the same port
			continue
		}
		seen[key] = true
		v.Ports = append(v.Ports, PortView{Public: p.PublicPort, Private: p.PrivatePort, Type: p.Type})
	}
	sort.Slice(v.Ports, func(i, j int) bool {
		if (v.Ports[i].Public == 0) != (v.Ports[j].Public == 0) {
			return v.Ports[i].Public != 0
		}
		return v.Ports[i].Private < v.Ports[j].Private
	})
	for n := range c.NetworkSettings.Networks {
		v.Networks = append(v.Networks, n)
	}
	sort.Strings(v.Networks)
	v.UpdateAvailable = s.updates.available(c.Image)
	if c.State == "running" {
		if u, ok := s.metrics.usageFor(c.ID); ok {
			v.CPU, v.Mem = &u.CPU, &u.Mem
		}
	}
	return v
}

// stackDirs lists folders in the stacks directory that contain a compose file.
func (s *Server) stackDirs() (string, map[string]string) {
	root := s.store.Settings().StacksDir
	out := map[string]string{} // name -> compose file
	entries, err := os.ReadDir(root)
	if err != nil {
		return root, out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if f := docker.FindComposeFile(filepath.Join(root, e.Name())); f != "" {
			out[e.Name()] = f
		}
	}
	return root, out
}

// buildStacks matches containers to stack folders. Compose projects that run
// from somewhere else show up as external stacks.
func (s *Server) buildStacks(ctx context.Context) ([]StackView, error) {
	containers, err := s.docker.Containers(ctx)
	if err != nil {
		return nil, err
	}
	root, dirs := s.stackDirs()
	byName := map[string]*StackView{}
	dirSet := map[string]string{}  // clean path -> name
	projSet := map[string]string{} // compose project -> name
	for name, file := range dirs {
		dir := filepath.Join(root, name)
		sv := &StackView{Name: name, Dir: dir, ComposeFile: file, Containers: []ContainerView{}}
		if st, err := os.Stat(filepath.Join(dir, file)); err == nil {
			sv.Modified = st.ModTime().Unix()
		}
		byName[name] = sv
		dirSet[filepath.Clean(dir)] = name
		projSet[composeProjectName(dir)] = name
	}
	external := map[string]*StackView{}
	for _, c := range containers {
		proj := c.Labels[docker.LabelProject]
		if proj == "" {
			continue
		}
		wd := filepath.Clean(c.Labels[docker.LabelWorkingDir])
		name, ok := dirSet[wd]
		if !ok {
			if n, ok2 := projSet[proj]; ok2 && !strings.HasPrefix(wd, filepath.Clean(root)+string(os.PathSeparator)) {
				name, ok = n, true
			}
		}
		var sv *StackView
		if ok {
			sv = byName[name]
		} else {
			sv = external[proj]
			if sv == nil {
				sv = &StackView{Name: proj, External: true, Containers: []ContainerView{}}
				if wd != "." {
					sv.Dir = wd
				}
				external[proj] = sv
			}
		}
		sv.Containers = append(sv.Containers, s.containerView(c))
	}
	out := make([]StackView, 0, len(byName)+len(external))
	for _, sv := range byName {
		out = append(out, *sv)
	}
	for _, sv := range external {
		out = append(out, *sv)
	}
	s.stackMu.Lock()
	for i := range out {
		finishStack(&out[i])
		if !out[i].External {
			out[i].Busy = s.stackBusy[out[i].Name]
		}
	}
	s.stackMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].External != out[j].External {
			return !out[i].External
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func finishStack(sv *StackView) {
	sort.Slice(sv.Containers, func(i, j int) bool { return sv.Containers[i].Name < sv.Containers[j].Name })
	sv.Total = len(sv.Containers)
	for _, c := range sv.Containers {
		if c.State == "running" {
			sv.Running++
		}
		if c.UpdateAvailable {
			sv.UpdateAvailable = true
		}
		if c.CPU != nil && c.Mem != nil {
			if sv.CPU == nil {
				sv.CPU, sv.Mem = new(float64), new(uint64)
			}
			*sv.CPU += *c.CPU
			*sv.Mem += *c.Mem
		}
	}
	switch {
	case sv.Total == 0:
		sv.Status = "inactive"
	case sv.Running == sv.Total:
		sv.Status = "running"
	case sv.Running == 0:
		sv.Status = "stopped"
	default:
		sv.Status = "partial"
	}
}

func (s *Server) findStack(ctx context.Context, name string) (*StackView, error) {
	stacks, err := s.buildStacks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range stacks {
		if stacks[i].Name == name {
			return &stacks[i], nil
		}
	}
	return nil, nil
}

// stackDir resolves a stack name to its folder, refusing anything that would
// escape the stacks directory.
func (s *Server) stackDir(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
		return "", errors.New("That isn't a valid stack name.")
	}
	return filepath.Join(s.store.Settings().StacksDir, name), nil
}

var newStackName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ---------- Handlers ----------

func (s *Server) listStacks(w http.ResponseWriter, r *http.Request) {
	s.metrics.want()
	stacks, err := s.buildStacks(r.Context())
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stacks)
}

func (s *Server) getStack(w http.ResponseWriter, r *http.Request) {
	sv, err := s.findStack(r.Context(), r.PathValue("name"))
	if err != nil {
		writeDockerErr(w, err)
		return
	}
	if sv == nil {
		writeErr(w, http.StatusNotFound, "That stack doesn't exist.")
		return
	}
	writeJSON(w, http.StatusOK, sv)
}

type stackFiles struct {
	Compose     string `json:"compose"`
	Env         string `json:"env"`
	ComposeFile string `json:"composeFile,omitempty"`
}

func (s *Server) getStackFiles(w http.ResponseWriter, r *http.Request) {
	dir, err := s.stackDir(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	file := docker.FindComposeFile(dir)
	if file == "" {
		writeErr(w, http.StatusNotFound, "That stack doesn't have a compose file.")
		return
	}
	compose, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't read the compose file.")
		return
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	writeJSON(w, http.StatusOK, stackFiles{Compose: string(compose), Env: string(env), ComposeFile: file})
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".homestack-tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) writeStackFiles(dir string, f stackFiles) error {
	file := docker.FindComposeFile(dir)
	if file == "" {
		file = "compose.yaml"
	}
	if err := writeFileAtomic(filepath.Join(dir, file), []byte(f.Compose), 0o644); err != nil {
		return err
	}
	envPath := filepath.Join(dir, ".env")
	if _, err := os.Stat(envPath); err == nil || strings.TrimSpace(f.Env) != "" {
		// .env often holds passwords, so keep it private.
		if err := writeFileAtomic(envPath, []byte(f.Env), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) putStackFiles(w http.ResponseWriter, r *http.Request) {
	dir, err := s.stackDir(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if docker.FindComposeFile(dir) == "" {
		writeErr(w, http.StatusNotFound, "That stack doesn't exist.")
		return
	}
	var f stackFiles
	if err := readJSON(r, &f); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(f.Compose) == "" {
		writeErr(w, http.StatusBadRequest, "The compose file can't be empty.")
		return
	}
	if err := s.writeStackFiles(dir, f); err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't save: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) validateStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dir, err := s.stackDir(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var f stackFiles
	if err := readJSON(r, &f); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := docker.ValidateCompose(ctx, dir, f.Compose, f.Env); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Compose string `json:"compose"`
		Env     string `json:"env"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !newStackName.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "Use lowercase letters, numbers, dashes and underscores for the name, starting with a letter or number.")
		return
	}
	if strings.TrimSpace(req.Compose) == "" {
		writeErr(w, http.StatusBadRequest, "The compose file can't be empty.")
		return
	}
	dir, _ := s.stackDir(req.Name)
	if _, err := os.Stat(dir); err == nil {
		writeErr(w, http.StatusConflict, "A stack with that name already exists.")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "Couldn't create the folder: "+err.Error())
		return
	}
	if err := s.writeStackFiles(dir, stackFiles{Compose: req.Compose, Env: req.Env}); err != nil {
		_ = os.RemoveAll(dir)
		writeErr(w, http.StatusInternalServerError, "Couldn't save: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": req.Name})
}

// lockStack marks a stack as busy so two actions can't run at once.
func (s *Server) lockStack(name, action string) (func(), bool) {
	s.stackMu.Lock()
	defer s.stackMu.Unlock()
	if _, busy := s.stackBusy[name]; busy {
		return nil, false
	}
	s.stackBusy[name] = action
	return func() {
		s.stackMu.Lock()
		delete(s.stackBusy, name)
		s.stackMu.Unlock()
	}, true
}

var stackActions = map[string][][]string{
	"up":      {{"up", "-d", "--remove-orphans"}},
	"stop":    {{"stop"}},
	"down":    {{"down", "--remove-orphans"}},
	"restart": {{"restart"}},
	"pull":    {{"pull"}},
	"update":  {{"pull"}, {"up", "-d", "--remove-orphans"}},
}

func (s *Server) stackAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	steps, ok := stackActions[action]
	if !ok {
		writeErr(w, http.StatusBadRequest, "Unknown action.")
		return
	}
	dir, err := s.stackDir(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if docker.FindComposeFile(dir) == "" {
		writeErr(w, http.StatusNotFound, "That stack doesn't exist.")
		return
	}
	unlock, ok := s.lockStack(name, action)
	if !ok {
		writeErr(w, http.StatusConflict, "Something else is already running on this stack. Wait for it to finish.")
		return
	}
	defer unlock()

	// Keep going even if the browser disconnects, so a deploy isn't cut off
	// halfway through.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	out := newNDJSON(w)
	tail := &tailBuffer{max: 8}
	emit := func(line string) { out.line(line); tail.add(line) }
	var runErr error
	for _, args := range steps {
		out.line("$ docker compose " + strings.Join(args, " "))
		if runErr = docker.Compose(ctx, dir, args, emit); runErr != nil {
			break
		}
	}
	s.notifyStackResult(name, action, runErr, tail.get())
	if runErr == nil && action == "update" {
		if s.store.Settings().PruneAfterUpdate {
			out.line("$ docker image prune")
			if rep, err := s.docker.ImagesPrune(ctx, false); err == nil {
				out.line(fmt.Sprintf("Removed %d old images, freed %s", rep.Deleted, humanBytes(rep.SpaceReclaimed)))
			} else {
				out.line("Couldn't prune old images: " + err.Error())
			}
		}
	}
	if runErr == nil && (action == "update" || action == "pull") {
		go s.updates.recheckStack(name)
	}
	out.done(runErr)
}

func (s *Server) deleteStack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dir, err := s.stackDir(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if docker.FindComposeFile(dir) == "" {
		writeErr(w, http.StatusNotFound, "That stack doesn't exist.")
		return
	}
	removeVolumes := r.URL.Query().Get("volumes") == "1"
	unlock, ok := s.lockStack(name, "delete")
	if !ok {
		writeErr(w, http.StatusConflict, "Something else is already running on this stack.")
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	out := newNDJSON(w)
	args := []string{"down", "--remove-orphans"}
	if removeVolumes {
		args = append(args, "--volumes")
	}
	out.line("$ docker compose " + strings.Join(args, " "))
	if err := docker.Compose(ctx, dir, args, out.line); err != nil {
		out.done(err)
		return
	}
	// Move the folder aside rather than deleting it, so app data kept in
	// bind mounts (./config, ./data) isn't lost by accident.
	trash := filepath.Join(s.store.Settings().StacksDir, ".trash")
	_ = os.MkdirAll(trash, 0o755)
	dest := filepath.Join(trash, fmt.Sprintf("%s-%s", name, time.Now().Format("20060102-150405")))
	if err := os.Rename(dir, dest); err != nil {
		out.done(fmt.Errorf("the stack is down, but its folder couldn't be moved: %v", err))
		return
	}
	out.line("Moved the stack folder to " + dest)
	out.done(nil)
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
