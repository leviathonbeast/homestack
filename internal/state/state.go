// Package state keeps everything Homestack needs to remember (the admin
// account, sessions and settings) in one small JSON file.
//
// A single admin doesn't need a database: the whole state is a few kilobytes,
// it is written atomically (write a temp file, then rename), and it is easy to
// back up or inspect by hand.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"homestack/internal/notify"
)

// Network is one entry in the access allowlist.
type Network struct {
	Name string `json:"name"`
	CIDR string `json:"cidr"`
}

// Admin is the single admin account.
type Admin struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	Changed      time.Time `json:"changed"`
}

// Session is a signed-in browser. Only a hash of the token is stored, so a
// leaked state file can't be used to hijack a session.
type Session struct {
	ID        string    `json:"id"`        // short public id used by the UI
	TokenHash string    `json:"tokenHash"` // sha256 of the cookie value
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"lastSeen"`
	Expires   time.Time `json:"expires"`
	UserAgent string    `json:"userAgent"`
	IP        string    `json:"ip"`
}

// Settings are everything editable from the settings page.
type Settings struct {
	StacksDir        string    `json:"stacksDir"`
	AllowedNetworks  []Network `json:"allowedNetworks"`
	ListenAddrs      []string  `json:"listenAddrs"` // empty = all interfaces
	SessionDays      int       `json:"sessionDays"`
	UpdateCheckHours int       `json:"updateCheckHours"` // 0 = off
	PruneAfterUpdate bool      `json:"pruneAfterUpdate"`
	Theme            string    `json:"theme"`          // system | light | dark
	VMDomainsDir     string    `json:"vmDomainsDir"`   // where new VM disks go
	VMIsosDir        string    `json:"vmIsosDir"`      // where installer ISOs are
	VMTemplatesDir   string    `json:"vmTemplatesDir"` // where VM template disks are kept
	// AppHost replaces [HOST] in links to apps' web pages. Empty = the
	// address the browser reached Homestack on. Set it when Homestack is
	// reached on a different address (a management port) than the apps.
	AppHost string `json:"appHost,omitempty"`
}

// Notifications holds the Discord webhook and which alerts are switched on.
// It lives outside Settings because the webhook URL is a secret: it is never
// sent back to the browser.
type Notifications struct {
	DiscordWebhook string          `json:"discordWebhook,omitempty"`
	Events         map[string]bool `json:"events,omitempty"` // missing = the event's default
	ServerName     string          `json:"serverName,omitempty"`
	// ntfy: the backup channel (push to the ntfy phone app)
	NtfyServer string `json:"ntfyServer,omitempty"`
	NtfyTopic  string `json:"ntfyTopic,omitempty"`
	NtfyToken  string `json:"ntfyToken,omitempty"`
	NtfyMode   string `json:"ntfyMode,omitempty"`
}

// NotifyConfig is what the notifier needs, with the hostname as the default
// server name.
func (n Notifications) NotifyConfig() notify.Config {
	name := n.ServerName
	if name == "" {
		name, _ = os.Hostname()
	}
	return notify.Config{Webhook: n.DiscordWebhook, Enabled: n.Events, ServerName: name,
		Ntfy: notify.Ntfy{Server: n.NtfyServer, Topic: n.NtfyTopic, Token: n.NtfyToken, Mode: n.NtfyMode}}
}

// AppLink is a shortcut on the dashboard to an app's web page. URL may use
// [HOST], which the browser replaces with Settings.AppHost, or else the
// address it reached Homestack on, so links work over the LAN and over
// WireGuard alike.
type AppLink struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Container string `json:"container,omitempty"` // for the running/stopped dot
}

// ContainerFolder groups containers on the Containers page, like Unraid's
// FolderView plugin. Containers are kept by name, which survives them being
// recreated; Match also pulls in any container whose name matches it.
type ContainerFolder struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Icon       string   `json:"icon,omitempty"`  // one of the web UI's icon names
	Color      string   `json:"color,omitempty"` // #rrggbb
	Match      string   `json:"match,omitempty"` // regular expression on container names
	Containers []string `json:"containers"`
	Collapsed  bool     `json:"collapsed,omitempty"`
}

// VMTemplate is a disk image new VMs are cloned from. The image itself lives
// in Settings.VMTemplatesDir on the host.
type VMTemplate struct {
	ID          string    `json:"id"` // also the image's file name, without .qcow2
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	OS          string    `json:"os"`       // linux | windows | other
	Firmware    string    `json:"firmware"` // uefi | bios
	CloudInit   bool      `json:"cloudInit"`
	Source      string    `json:"source"` // what it was made from: a cloud image, a URL or a VM
	Path        string    `json:"path"`
	Checksum    string    `json:"checksum,omitempty"` // of the download, verified
	Created     time.Time `json:"created"`
}

// CloudInitDefaults pre-fills the login on the next VM made from a
// template. Public keys only; passwords are never kept.
type CloudInitDefaults struct {
	User    string   `json:"user,omitempty"`
	SSHKeys []string `json:"sshKeys,omitempty"`
}

// State is the whole file.
type State struct {
	Version       int               `json:"version"`
	Admin         *Admin            `json:"admin,omitempty"`
	Sessions      []Session         `json:"sessions"`
	Settings      Settings          `json:"settings"`
	Notifications Notifications     `json:"notifications"`
	Apps          []AppLink         `json:"apps"`
	AppsSaved     bool              `json:"appsSaved,omitempty"` // the admin has edited the list at least once
	Folders       []ContainerFolder `json:"folders,omitempty"`
	VMTemplates   []VMTemplate      `json:"vmTemplates,omitempty"`
	CloudInit     CloudInitDefaults `json:"cloudInit,omitzero"`
}

// DefaultNetworks covers loopback, the private LAN ranges and the CGNAT range
// that Tailscale and some WireGuard meshes use. Nothing on the public internet
// is allowed.
func DefaultNetworks() []Network {
	return []Network{
		{Name: "This machine", CIDR: "127.0.0.0/8"},
		{Name: "This machine (IPv6)", CIDR: "::1/128"},
		{Name: "Private LAN (10.x)", CIDR: "10.0.0.0/8"},
		{Name: "Private LAN (172.16.x)", CIDR: "172.16.0.0/12"},
		{Name: "Private LAN (192.168.x)", CIDR: "192.168.0.0/16"},
		{Name: "Mesh VPN (100.64.x)", CIDR: "100.64.0.0/10"},
		{Name: "Private IPv6", CIDR: "fc00::/7"},
	}
}

// Defaults returns the settings a fresh install starts with.
func Defaults(stacksDir string) Settings {
	return Settings{
		StacksDir:        stacksDir,
		AllowedNetworks:  DefaultNetworks(),
		ListenAddrs:      nil,
		SessionDays:      30,
		UpdateCheckHours: 12,
		PruneAfterUpdate: false,
		Theme:            "system",
		VMDomainsDir:     "/mnt/user/domains",
		VMIsosDir:        "/mnt/user/isos",
		VMTemplatesDir:   "/mnt/user/domains/templates",
	}
}

// Store guards the state and persists every change.
type Store struct {
	mu      sync.RWMutex
	path    string
	s       State
	lastMod time.Time // mtime after our last write, to spot outside edits
	defDir  string
}

// Open loads the state file, creating it with defaults if it doesn't exist.
func Open(path, defaultStacksDir string) (*Store, error) {
	st := &Store{path: path, defDir: defaultStacksDir}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.s = State{Version: 1, Settings: Defaults(defaultStacksDir)}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := st.saveLocked(); err != nil {
			return nil, err
		}
		return st, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &st.s); err != nil {
		return nil, fmt.Errorf("state file %s is corrupt: %w", path, err)
	}
	st.fillDefaults(defaultStacksDir)
	if fi, err := os.Stat(path); err == nil {
		st.lastMod = fi.ModTime()
	}
	return st, nil
}

// Restore replaces the state with a backup's (its homestack.json). Every
// session ends: the backup's would be stale, and whoever restores signs in
// again with the restored account.
func (st *Store) Restore(data []byte) error {
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("the backup's Homestack settings aren't readable: %w", err)
	}
	if s.Version < 1 || s.Admin == nil || s.Admin.Username == "" || s.Admin.PasswordHash == "" {
		return errors.New("the backup's Homestack settings have no account in them")
	}
	s.Sessions = []Session{}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s = s
	st.fillDefaults(st.defDir)
	return st.saveLocked()
}

// ReloadIfChanged re-reads the file if something else changed it (for
// example `homestack reset-access` run while the server is up). It reports
// whether a reload happened.
func (st *Store) ReloadIfChanged() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.reloadIfChangedLocked()
}

func (st *Store) reloadIfChangedLocked() bool {
	fi, err := os.Stat(st.path)
	if err != nil || fi.ModTime().Equal(st.lastMod) {
		return false
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return false
	}
	var fresh State
	if json.Unmarshal(data, &fresh) != nil {
		return false
	}
	st.s = fresh
	st.fillDefaults(st.defDir)
	st.lastMod = fi.ModTime()
	return true
}

func (st *Store) fillDefaults(defaultStacksDir string) {
	s := &st.s.Settings
	if s.StacksDir == "" {
		s.StacksDir = defaultStacksDir
	}
	if s.SessionDays <= 0 {
		s.SessionDays = 30
	}
	if s.Theme == "" {
		s.Theme = "system"
	}
	if s.VMDomainsDir == "" {
		s.VMDomainsDir = "/mnt/user/domains"
	}
	if s.VMIsosDir == "" {
		s.VMIsosDir = "/mnt/user/isos"
	}
	if s.VMTemplatesDir == "" {
		s.VMTemplatesDir = strings.TrimRight(s.VMDomainsDir, "/") + "/templates"
	}
}

// Notifications returns a copy of the notification settings.
func (st *Store) Notifications() Notifications {
	st.mu.RLock()
	defer st.mu.RUnlock()
	n := st.s.Notifications
	ev := make(map[string]bool, len(n.Events))
	maps.Copy(ev, n.Events)
	n.Events = ev
	return n
}

// Path is where the state lives on disk.
func (st *Store) Path() string { return st.path }

// Read gives read-only access to the state. Don't keep references to slices.
func (st *Store) Read(fn func(s *State)) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	fn(&st.s)
}

// Settings returns a copy of the current settings.
func (st *Store) Settings() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	c := st.s.Settings
	c.AllowedNetworks = append([]Network(nil), c.AllowedNetworks...)
	c.ListenAddrs = append([]string(nil), c.ListenAddrs...)
	return c
}

// Update changes the state and saves it. If fn returns an error nothing is saved.
func (st *Store) Update(fn func(s *State) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reloadIfChangedLocked()
	backup, _ := json.Marshal(st.s)
	if err := fn(&st.s); err != nil {
		_ = json.Unmarshal(backup, &st.s)
		return err
	}
	if err := st.saveLocked(); err != nil {
		_ = json.Unmarshal(backup, &st.s)
		return err
	}
	return nil
}

func (st *Store) saveLocked() error {
	data, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return err
	}
	if fi, err := os.Stat(st.path); err == nil {
		st.lastMod = fi.ModTime()
	}
	return nil
}
