// Package server is Homestack's HTTP API and web UI host.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"homestack/internal/docker"
	"homestack/internal/libvirt"
	"homestack/internal/notify"
	"homestack/internal/state"
)

// Config comes from environment variables and flags; everything else is in
// the state file and editable from the UI.
type Config struct {
	Port           string       // HOMESTACK_PORT
	TrustedProxies []*net.IPNet // HOMESTACK_TRUSTED_PROXIES
	SecureCookie   bool         // HOMESTACK_SECURE_COOKIE
	Version        string
	Source         string // where this build's source code is (AGPL)
}

// Server holds everything the handlers need.
type Server struct {
	cfg       Config
	store     *state.Store
	docker    *docker.Client
	vms       *libvirt.Client
	web       fs.FS
	listeners *listenerMgr
	limiter   *loginLimiter
	updates   *updateChecker
	access    pendingAccess

	stackMu   sync.Mutex
	stackBusy map[string]string // stack name -> action in progress

	notifier      *notify.Notifier
	cwatch        *containerWatch
	updNotifiedMu sync.Mutex
	updNotified   map[string]bool // image refs already reported as having an update
	vmIntentMu    sync.Mutex
	vmIntent      map[string]time.Time // VM name -> last power action from Homestack
	icons         *iconCache
	metrics       *metrics
	netUsage      netUsageCache
	tpl           tplState
	vmstats       *vmStats
}

// New wires the server together.
func New(cfg Config, store *state.Store, dc *docker.Client, web fs.FS) *Server {
	s := &Server{
		cfg:         cfg,
		store:       store,
		docker:      dc,
		vms:         libvirt.New(),
		web:         web,
		limiter:     newLoginLimiter(),
		stackBusy:   map[string]string{},
		cwatch:      newContainerWatch(),
		updNotified: map[string]bool{},
		vmIntent:    map[string]time.Time{},
		icons:       newIconCache(),
		metrics:     newMetrics(),
		vmstats:     newVMStats(),
	}
	s.notifier = notify.New(s.notifyConfig)
	s.updates = newUpdateChecker(s)
	s.listeners = newListenerMgr(cfg.Port, s.routes())
	return s
}

// Run starts the listeners and background jobs, and blocks until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.store.Settings().StacksDir, 0o755); err != nil {
		log.Printf("warning: can't create stacks folder: %v", err)
	}
	s.listeners.Reconcile(s.store.Settings().ListenAddrs)
	go s.updates.loop(ctx)
	go s.sessionJanitor(ctx)
	go s.watchStateFile(ctx)
	go s.notifier.Run(ctx)
	go s.watchContainers(ctx)
	go s.watchVMs(ctx)
	go s.announceStart(ctx)
	go s.sampleHost(ctx)
	go s.sampleContainers(ctx)
	go s.sampleVMs(ctx)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.listeners.Shutdown(shutdownCtx)
	return nil
}

// watchStateFile applies changes made by the recovery commands
// (reset-access, reset-password) without needing a restart.
func (s *Server) watchStateFile(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.store.ReloadIfChanged() {
				log.Printf("state file changed on disk, reloaded it")
				s.listeners.Reconcile(s.store.Settings().ListenAddrs)
			}
		}
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	a := s.requireAuth

	// Public
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/session", s.getSession)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/preflight", s.preflight) // public only until setup is done

	// Account and settings
	mux.HandleFunc("GET /api/account/sessions", a(s.listSessions))
	mux.HandleFunc("DELETE /api/account/sessions/{id}", a(s.revokeSession))
	mux.HandleFunc("POST /api/account/sessions/revoke-others", a(s.revokeOtherSessions))
	mux.HandleFunc("POST /api/account/password", a(s.changePassword))
	mux.HandleFunc("POST /api/account/username", a(s.changeUsername))
	mux.HandleFunc("GET /api/settings", a(s.getSettings))
	mux.HandleFunc("PUT /api/settings/general", a(s.putGeneralSettings))
	mux.HandleFunc("GET /api/settings/access", a(s.getAccess))
	mux.HandleFunc("PUT /api/settings/access", a(s.putAccess))
	mux.HandleFunc("POST /api/settings/access/confirm", a(s.confirmAccess))
	mux.HandleFunc("POST /api/settings/access/cancel", a(s.cancelAccess))
	mux.HandleFunc("GET /api/settings/notifications", a(s.getNotifications))
	mux.HandleFunc("PUT /api/settings/notifications", a(s.putNotifications))
	mux.HandleFunc("POST /api/settings/notifications/test", a(s.testNotification))

	// Overview
	mux.HandleFunc("GET /api/dashboard", a(s.dashboard))
	mux.HandleFunc("GET /api/events", a(s.events))
	mux.HandleFunc("GET /api/templates", a(s.listTemplates))
	mux.HandleFunc("GET /api/updates", a(s.getUpdates))
	mux.HandleFunc("GET /api/apps", a(s.listApps))
	mux.HandleFunc("PUT /api/apps", a(s.putApps))
	mux.HandleFunc("PUT /api/apps/container", a(s.setContainerWebUI))
	mux.HandleFunc("GET /api/apps/{id}/icon", a(s.appIcon))
	mux.HandleFunc("POST /api/updates/check", a(s.checkUpdates))

	// Stacks
	mux.HandleFunc("GET /api/stacks", a(s.listStacks))
	mux.HandleFunc("POST /api/stacks", a(s.createStack))
	mux.HandleFunc("GET /api/stacks/{name}", a(s.getStack))
	mux.HandleFunc("DELETE /api/stacks/{name}", a(s.deleteStack))
	mux.HandleFunc("GET /api/stacks/{name}/files", a(s.getStackFiles))
	mux.HandleFunc("PUT /api/stacks/{name}/files", a(s.putStackFiles))
	mux.HandleFunc("POST /api/stacks/{name}/validate", a(s.validateStack))
	mux.HandleFunc("POST /api/stacks/{name}/action/{action}", a(s.stackAction))
	mux.HandleFunc("GET /api/stacks/{name}/logs", a(s.stackLogs))
	mux.HandleFunc("GET /api/stacks/{name}/stats", a(s.stackStats))

	// Host service: storage, parity, array and fans
	mux.HandleFunc("GET /api/host/available", a(s.hostAvailable))
	mux.HandleFunc("GET /api/host/status", a(s.proxyAgent("GET", "/v1/status")))
	mux.HandleFunc("POST /api/host/actions", a(s.proxyAgent("POST", "/v1/actions")))
	mux.HandleFunc("GET /api/host/jobs/{id}/log", a(s.proxyAgent("GET", "")))
	mux.HandleFunc("GET /api/host/runs", a(s.proxyAgent("GET", "/v1/runs")))
	mux.HandleFunc("GET /api/host/runs/log", a(s.proxyAgent("GET", "/v1/runs/log")))
	mux.HandleFunc("GET /api/host/disks/smart", a(s.proxyAgent("GET", "/v1/disks/smart")))
	mux.HandleFunc("GET /api/host/mover", a(s.proxyAgent("GET", "/v1/mover")))
	mux.HandleFunc("PUT /api/host/mover", a(s.proxyAgent("PUT", "/v1/mover")))
	mux.HandleFunc("GET /api/host/fans/config", a(s.proxyAgent("GET", "/v1/fans/config")))
	mux.HandleFunc("PUT /api/host/fans/config", a(s.proxyAgent("PUT", "/v1/fans/config")))
	mux.HandleFunc("POST /api/host/fans/test", a(s.proxyAgent("POST", "/v1/fans/test")))
	mux.HandleFunc("GET /api/host/schedules", a(s.proxyAgent("GET", "/v1/schedules")))
	mux.HandleFunc("PUT /api/host/schedules", a(s.proxyAgent("PUT", "/v1/schedules")))
	mux.HandleFunc("GET /api/host/shares", a(s.proxyAgent("GET", "/v1/shares")))
	mux.HandleFunc("POST /api/host/shares", a(s.proxyAgent("POST", "/v1/shares")))
	mux.HandleFunc("GET /api/host/files", a(s.proxyAgent("GET", "/v1/files")))
	mux.HandleFunc("POST /api/host/files", a(s.proxyAgent("POST", "/v1/files")))
	mux.HandleFunc("POST /api/host/files/plan", a(s.proxyAgent("POST", "/v1/files/plan")))
	mux.HandleFunc("GET /api/host/zfs/arc", a(s.proxyAgent("GET", "/v1/zfs/arc")))
	mux.HandleFunc("PUT /api/host/zfs/arc", a(s.proxyAgent("PUT", "/v1/zfs/arc")))
	mux.HandleFunc("GET /api/host/files/download", a(s.downloadFromAgent))
	mux.HandleFunc("GET /api/host/files/content", a(s.proxyAgent("GET", "/v1/files/content")))
	mux.HandleFunc("PUT /api/host/files/content", a(s.proxyAgent("PUT", "/v1/files/content")))
	mux.HandleFunc("GET /api/host/replace", a(s.proxyAgent("GET", "/v1/replace")))
	mux.HandleFunc("POST /api/host/replace", a(s.proxyAgent("POST", "/v1/replace")))
	mux.HandleFunc("GET /api/host/array", a(s.proxyAgent("GET", "/v1/array")))
	mux.HandleFunc("POST /api/host/array", a(s.proxyAgent("POST", "/v1/array")))
	mux.HandleFunc("GET /api/host/spindown", a(s.proxyAgent("GET", "/v1/spindown")))
	mux.HandleFunc("PUT /api/host/spindown", a(s.proxyAgent("PUT", "/v1/spindown")))
	mux.HandleFunc("GET /api/host/parity", a(s.proxyAgent("GET", "/v1/parity")))
	mux.HandleFunc("PUT /api/host/parity", a(s.proxyAgent("PUT", "/v1/parity")))
	mux.HandleFunc("GET /api/host/storage/layout", a(s.proxyAgent("GET", "/v1/storage/layout")))
	mux.HandleFunc("PUT /api/host/storage/layout", a(s.proxyAgent("PUT", "/v1/storage/layout")))
	mux.HandleFunc("GET /api/backups", a(s.proxyAgent("GET", "/v1/backups")))
	mux.HandleFunc("PUT /api/backups/settings", a(s.proxyAgent("PUT", "/v1/backups/settings")))
	mux.HandleFunc("POST /api/backups", a(s.proxyAgent("POST", "/v1/backups")))
	mux.HandleFunc("GET /api/backups/file", a(s.backupFile))
	mux.HandleFunc("POST /api/backups/open", s.setupOrAuth(s.openBackup))
	mux.HandleFunc("POST /api/backups/restore", s.setupOrAuth(s.restoreBackup))

	// Virtual machines
	mux.HandleFunc("GET /api/vms/status", a(s.vmStatus))
	mux.HandleFunc("GET /api/vms/options", a(s.vmOptions))
	mux.HandleFunc("GET /api/vm-templates", a(s.listVMTemplates))
	mux.HandleFunc("POST /api/vm-templates", a(s.addVMTemplate))
	mux.HandleFunc("PUT /api/vm-templates/{id}", a(s.putVMTemplate))
	mux.HandleFunc("DELETE /api/vm-templates/{id}", a(s.deleteVMTemplate))
	mux.HandleFunc("GET /api/vms", a(s.listVMs))
	mux.HandleFunc("POST /api/vms", a(s.createVM))
	mux.HandleFunc("GET /api/vms/{name}", a(s.getVM))
	mux.HandleFunc("DELETE /api/vms/{name}", a(s.deleteVM))
	mux.HandleFunc("POST /api/vms/{name}/action/{action}", a(s.vmAction))
	mux.HandleFunc("POST /api/vms/{name}/autostart", a(s.vmAutostart))
	mux.HandleFunc("GET /api/vms/{name}/xml", a(s.getVMXML))
	mux.HandleFunc("PUT /api/vms/{name}/xml", a(s.putVMXML))
	mux.HandleFunc("GET /api/vms/{name}/config", a(s.getVMConfig))
	mux.HandleFunc("PUT /api/vms/{name}/config", a(s.putVMConfig))
	mux.HandleFunc("GET /api/vms/{name}/console", a(s.vmConsole))
	mux.HandleFunc("GET /api/vms/{name}/serial", a(s.vmSerial))
	mux.HandleFunc("GET /api/vms/{name}/metrics", a(s.vmMetrics))

	// Containers
	mux.HandleFunc("GET /api/containers", a(s.listContainers))
	mux.HandleFunc("GET /api/containers/folders", a(s.getFolders))
	mux.HandleFunc("PUT /api/containers/folders", a(s.putFolders))
	mux.HandleFunc("GET /api/containers/{id}", a(s.getContainer))
	mux.HandleFunc("POST /api/containers/{id}/action/{action}", a(s.containerAction))
	mux.HandleFunc("DELETE /api/containers/{id}", a(s.removeContainer))
	mux.HandleFunc("GET /api/containers/{id}/logs", a(s.containerLogs))
	mux.HandleFunc("GET /api/containers/{id}/stats", a(s.containerStats))
	mux.HandleFunc("GET /api/containers/{id}/exec", a(s.containerExec))

	// Images, volumes, networks
	mux.HandleFunc("GET /api/images", a(s.listImages))
	mux.HandleFunc("POST /api/images/pull", a(s.pullImage))
	mux.HandleFunc("POST /api/images/prune", a(s.pruneImages))
	mux.HandleFunc("DELETE /api/images/{id}", a(s.removeImage))
	mux.HandleFunc("GET /api/volumes", a(s.listVolumes))
	mux.HandleFunc("POST /api/volumes", a(s.createVolume))
	mux.HandleFunc("POST /api/volumes/prune", a(s.pruneVolumes))
	mux.HandleFunc("DELETE /api/volumes/{name}", a(s.removeVolume))
	mux.HandleFunc("GET /api/networks", a(s.listNetworks))
	mux.HandleFunc("POST /api/networks", a(s.createNetwork))
	mux.HandleFunc("POST /api/networks/prune", a(s.pruneNetworks))
	mux.HandleFunc("DELETE /api/networks/{id}", a(s.removeNetwork))
	mux.HandleFunc("GET /api/networks/parents", a(s.networkParents))

	// The server's own network (through the host agent) and the VMs' networks
	mux.HandleFunc("GET /api/network/host", a(s.netHost))
	mux.HandleFunc("GET /api/network/host/file", a(s.netHostFile))
	mux.HandleFunc("POST /api/network/host/plan", a(s.netHostChange(false)))
	mux.HandleFunc("POST /api/network/host/apply", a(s.netHostChange(true)))
	mux.HandleFunc("POST /api/network/host/confirm", a(s.netHostConfirm))
	mux.HandleFunc("POST /api/network/host/cancel", a(s.netHostCancel))
	mux.HandleFunc("GET /api/network/vms", a(s.netVMs))
	mux.HandleFunc("POST /api/network/vms", a(s.createVMNet))
	mux.HandleFunc("POST /api/network/vms/{name}/action", a(s.vmNetAction))
	mux.HandleFunc("DELETE /api/network/vms/{name}", a(s.deleteVMNet))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "Not found.")
	})
	mux.Handle("/", s.spa())

	return s.securityHeaders(s.checkAccess(s.checkCSRF(mux)))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	dockerOK := s.docker.Ping(ctx) == nil
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "docker": dockerOK, "version": s.cfg.Version})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeDockerErr turns a Docker error into a friendly API error.
func writeDockerErr(w http.ResponseWriter, err error) {
	if ae, ok := errors.AsType[*docker.APIError](err); ok {
		status := ae.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		writeErr(w, status, sentence(ae.Message))
		return
	}
	writeErr(w, http.StatusBadGateway, sentence(err.Error()))
}

func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("That request wasn't valid JSON.")
	}
	return nil
}

// ndjson streams progress lines (compose output, image pulls) to the browser.
type ndjson struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu sync.Mutex
}

func newNDJSON(w http.ResponseWriter) *ndjson {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &ndjson{w: w, f: f}
}

func (n *ndjson) send(v any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	b, _ := json.Marshal(v)
	_, _ = n.w.Write(append(b, '\n'))
	if n.f != nil {
		n.f.Flush()
	}
}

func (n *ndjson) line(text string) { n.send(map[string]string{"type": "line", "text": text}) }

func (n *ndjson) done(err error) {
	if err != nil {
		n.send(map[string]any{"type": "done", "ok": false, "error": sentence(err.Error())})
		return
	}
	n.send(map[string]any{"type": "done", "ok": true})
}
