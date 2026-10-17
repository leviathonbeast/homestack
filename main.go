// Homestack: a lightweight web UI for managing Docker Compose stacks in a
// home lab.
//
// Usage:
//
//	homestack                 start the server
//	homestack reset-access    allow all private networks and every interface again
//	homestack reset-password  set a new random admin password and sign out everywhere
//	homestack healthcheck     exit 0 if the server is up (used by Docker)
//	homestack notify          send a message to the Discord channel (for scripts)
//	homestack agent           the host service: fans, storage status, parity and array jobs
//	homestack version
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"homestack/internal/agent"
	"homestack/internal/docker"
	"homestack/internal/notify"
	"homestack/internal/server"
	"homestack/internal/state"
	"homestack/web"
)

// version is set at build time with -ldflags "-X main.version=..."
var version = "dev"

// source is where this build's source code is. Homestack is AGPL: anyone
// running a changed copy for other people must offer them its source, so a
// fork sets its own with -ldflags "-X main.source=https://…".
var source = "https://github.com/leviathonbeast/homestack"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// defaultDataDir picks a sensible place for homestack.json, so the recovery
// commands find it without extra flags.
func defaultDataDir() string {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "/data"
	}
	if os.Geteuid() == 0 {
		return "/var/lib/homestack"
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "homestack")
	}
	return "./data"
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	dataDir := env("HOMESTACK_DATA", defaultDataDir())
	statePath := filepath.Join(dataDir, "homestack.json")
	stacksDir := env("HOMESTACK_STACKS", "/opt/stacks")
	port := env("HOMESTACK_PORT", "5080")

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		serve(statePath, stacksDir, port)
	case "reset-access":
		resetAccess(statePath, stacksDir)
	case "reset-password":
		resetPassword(statePath, stacksDir)
	case "healthcheck":
		healthcheck(statePath, stacksDir, port)
	case "notify":
		notifyCmd(statePath, os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "backup":
		backupCmd()
	case "version", "--version", "-v":
		fmt.Println("homestack", version)
	case "help", "--help", "-h":
		fmt.Println(`Usage:
  homestack                 start the server
  homestack reset-access    allow all private networks and every interface again
  homestack reset-password  set a new random admin password and sign out everywhere
  homestack healthcheck     exit 0 if the server is up
  homestack notify [-level info|success|warning|error] [-title TEXT] [MESSAGE]
                            send a message to the Discord channel set up in
                            Settings → Notifications (reads MESSAGE from stdin
                            if it isn't given). Does nothing if notifications
                            are off, so scripts can always call it.
  homestack agent           run the host service (fans, storage, parity, array
                            controls). Runs on the server itself as root, from
                            systemd; see deploy/debian/agent/.
  homestack agent restore-fans
                            hand every fan header back to the BIOS
  homestack agent restore-network
                            undo a network change that wasn't kept
  homestack backup          ask the host service to save a backup now (the
                            nightly Backup schedule runs this)
  homestack version

Environment:
  HOMESTACK_PORT            port to listen on (default 5080)
  HOMESTACK_DATA            folder for homestack.json (default /data in Docker,
                            /var/lib/homestack as root, else ~/.config/homestack)
  HOMESTACK_STACKS          stacks folder used on first start (default /opt/stacks)
  HOMESTACK_TRUSTED_PROXIES comma-separated proxy IPs/ranges allowed to set X-Forwarded-For
  HOMESTACK_SECURE_COOKIE   set to true when served over HTTPS by a reverse proxy
  DOCKER_HOST               unix:///var/run/docker.sock (default) or tcp://host:port`)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (try `homestack help`)\n", cmd)
		os.Exit(2)
	}
}

func serve(statePath, stacksDir, port string) {
	store, err := state.Open(statePath, stacksDir)
	if err != nil {
		log.Fatalf("can't open state: %v", err)
	}
	dc, err := docker.New()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := dc.Ping(pingCtx); err != nil {
		log.Printf("warning: %v (Homestack will keep trying)", err)
	}
	cancel()

	var trusted []*net.IPNet
	for p := range strings.SplitSeq(os.Getenv("HOMESTACK_TRUSTED_PROXIES"), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			if strings.Contains(p, ":") {
				p += "/128"
			} else {
				p += "/32"
			}
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			log.Fatalf("HOMESTACK_TRUSTED_PROXIES: %q isn't a valid IP or range", p)
		}
		trusted = append(trusted, n)
	}

	cfg := server.Config{
		Port:           port,
		TrustedProxies: trusted,
		SecureCookie:   os.Getenv("HOMESTACK_SECURE_COOKIE") == "true",
		Version:        version,
		Source:         source,
	}
	var needsSetup bool
	store.Read(func(s *state.State) { needsSetup = s.Admin == nil })
	log.Printf("Homestack %s starting (state: %s, stacks: %s)", version, statePath, store.Settings().StacksDir)
	if needsSetup {
		log.Printf("no admin account yet: open the web UI to create one")
	}
	srv := server.New(cfg, store, dc, web.FS())
	if err := srv.Run(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("stopped")
}

func resetAccess(statePath, stacksDir string) {
	store, err := state.Open(statePath, stacksDir)
	if err != nil {
		log.Fatalf("can't open state: %v", err)
	}
	err = store.Update(func(s *state.State) error {
		s.Settings.AllowedNetworks = state.DefaultNetworks()
		s.Settings.ListenAddrs = nil
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Access reset: every private network is allowed and Homestack listens on all interfaces.")
	fmt.Println("A running server picks this up within a few seconds.")
}

func resetPassword(statePath, stacksDir string) {
	store, err := state.Open(statePath, stacksDir)
	if err != nil {
		log.Fatalf("can't open state: %v", err)
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	pw := base64.RawURLEncoding.EncodeToString(b)
	hash, err := state.HashPassword(pw)
	if err != nil {
		log.Fatal(err)
	}
	var username string
	err = store.Update(func(s *state.State) error {
		if s.Admin == nil {
			return fmt.Errorf("there's no admin account yet; open the web UI to create one")
		}
		s.Admin.PasswordHash = hash
		s.Admin.Changed = time.Now()
		s.Sessions = nil
		username = s.Admin.Username
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Password reset. Sign in as %q with this password, then change it in Settings:\n\n  %s\n\n", username, pw)
}

func healthcheck(statePath, stacksDir, port string) {
	host := "127.0.0.1"
	if store, err := state.Open(statePath, stacksDir); err == nil {
		if addrs := store.Settings().ListenAddrs; len(addrs) > 0 {
			host = addrs[0]
		}
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/api/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

// notifyCmd lets the server's own scripts (ZFS, SMART, disk space, the mover)
// post to the same Discord channel, using the webhook saved in the UI.
func notifyCmd(statePath string, args []string) {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	level := fs.String("level", "info", "info, success, warning or error")
	title := fs.String("title", "", "message title")
	event := fs.String("event", "host", "notification type, for the on/off switches")
	_ = fs.Parse(args)
	msg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if msg == "" {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			b, _ := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
			msg = strings.TrimSpace(string(b))
		}
	}
	if msg == "" && *title == "" {
		fmt.Fprintln(os.Stderr, "usage: homestack notify [-level L] [-title T] MESSAGE")
		os.Exit(2)
	}
	if *title == "" {
		*title, msg = msg, ""
		if i := strings.IndexByte(*title, '\n'); i > 0 {
			*title, msg = (*title)[:i], (*title)[i+1:]
		}
	}
	// Read the state file directly; never create one.
	if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "homestack notify: no settings at %s (set HOMESTACK_DATA)\n", statePath)
		os.Exit(1)
	}
	store, err := state.Open(statePath, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "homestack notify:", err)
		os.Exit(1)
	}
	cfg := store.Notifications().NotifyConfig()
	if !cfg.Active() || !cfg.EventEnabled(*event) {
		return // notifications are off: nothing to do, and not an error
	}
	m := notify.Message{Event: *event, Level: notify.ParseLevel(*level), Title: *title, Body: msg}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := notify.DeliverNow(ctx, cfg, m); err != nil {
		fmt.Fprintln(os.Stderr, "homestack notify:", err)
		os.Exit(1)
	}
}

// agentCmd runs the host service. It needs root and the real hardware, so it
// runs from systemd on the server, never inside Docker.
// backupCmd asks the host agent to save a backup (the nightly schedule's
// command). The agent records the result and alerts on a failure; this
// just reports it and exits non-zero if it failed.
func backupCmd() {
	sock := env("HOMESTACK_AGENT_SOCKET", "/run/homestack/agent.sock")
	c := &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := c.Post("http://agent/v1/backups", "application/json", nil)
	if err != nil {
		log.Fatalf("couldn't reach the host agent at %s: %v", sock, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("backup failed: %s", strings.TrimSpace(string(b)))
	}
	fmt.Println(strings.TrimSpace(string(b)))
}

func agentCmd(args []string) {
	opt := agent.Options{
		Socket: env("HOMESTACK_AGENT_SOCKET", "/run/homestack/agent.sock"),
		// The real folder on the cache (install.sh works it out from the
		// container): the apps' data mount goes away while the array is
		// stopped, and the agent still needs the Discord settings.
		DataDir:    env("HOMESTACK_DATA", "/mnt/cache/appdata/homestack"),
		Source:     os.Getenv("HOMESTACK_SRC"),
		StateDir:   env("HOMESTACK_AGENT_STATE", "/var/lib/homestack-agent"),
		LogDir:     env("HOMESTACK_AGENT_LOGS", "/var/log/homestack-agent"),
		ScriptsDir: env("HOMESTACK_AGENT_SCRIPTS", "/usr/local/sbin"),
		LockFile:   env("HOMESTACK_AGENT_LOCK", "/run/lock/homestack-array.lock"),
		SysRoot:    os.Getenv("HOMESTACK_AGENT_SYSROOT"),
		Version:    version,
	}
	if len(args) > 0 && args[0] == "restore-fans" {
		if err := agent.RestoreFans(opt); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Fan headers handed back to the BIOS.")
		return
	}
	if len(args) > 0 && args[0] == "restore-network" {
		if err := agent.RestoreNetwork(opt); err != nil {
			log.Fatal(err)
		}
		return
	}
	if os.Geteuid() != 0 {
		log.Fatal("homestack agent must run as root (it controls fans and storage)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := agent.New(opt).Run(ctx); err != nil {
		log.Fatal(err)
	}
}
