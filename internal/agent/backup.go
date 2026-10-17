package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"homestack/internal/backup"
	"homestack/internal/notify"
)

// Backups (Settings → Backups): Homestack's settings and everything it set
// up on the server, in one encrypted file (internal/backup), for when the
// boot drive dies. The agent makes them, nightly (Schedules → Backup) or on
// demand, into a folder off the boot drive, keeping the newest few. The
// Homestack server opens one (it's given the passphrase; the agent only
// keeps the key derived from it, to encrypt) and hands the agent the
// server's parts to restore. Each part's files go to fixed places: nothing
// in a backup can name where it's written.

const (
	backupKeep  = 14
	passdbPath  = "/var/lib/samba/private/passdb.tdb"
	nvramDir    = "/var/lib/libvirt/qemu/nvram/"
	fansModules = "/etc/modules-load.d/homestack-fans.conf"
	upsDefaults = "/etc/default/homestack-ups"

	dockerDaemonJSON = "/etc/docker/daemon.json"
	waitDropIn       = "wait-for-storage.conf"
)

var reUnitDir = regexp.MustCompile(`^[A-Za-z0-9@._-]+\.service\.d$`)

// The UPS files the UPS installer writes (deploy/debian/ups).
var nutFiles = []string{"nut.conf", "ups.conf", "upsd.conf", "upsd.users", "upsmon.conf", "upssched.conf"}

var reBackupName = regexp.MustCompile(`^homestack-backup-[A-Za-z0-9.-]+-\d{8}-\d{6}\.hsbak$`)

type backupSettings struct {
	Dir  string      `json:"dir"`
	Keep int         `json:"keep"`
	Key  *backup.Key `json:"key,omitempty"`
}

func (a *Agent) backupSettingsPath() string { return filepath.Join(a.opt.StateDir, "backup.json") }

// defaultBackupDir is a folder in the shares: on the cache or the array,
// never the boot drive.
func (a *Agent) defaultBackupDir() string { return a.layout().Shares + "/backups/homestack" }

func (a *Agent) loadBackupSettings() backupSettings {
	s := backupSettings{Keep: backupKeep}
	if b, err := os.ReadFile(a.backupSettingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Keep <= 0 {
		s.Keep = backupKeep
	}
	if s.Dir == "" {
		s.Dir = a.defaultBackupDir()
	}
	return s
}

func (a *Agent) saveBackupSettings(s backupSettings) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := a.backupSettingsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { // the key is in it
		return err
	}
	return os.Rename(tmp, a.backupSettingsPath())
}

// BackupFile is one saved backup.
type BackupFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	Dir     string    `json:"dir"`          // where it is
	On      string    `json:"on,omitempty"` // which disk that is, when it's one ("the cache", "disk3")
	// Set when it's made:
	Contents []string `json:"contents,omitempty"` // its parts, with counts
	Missing  []string `json:"missing,omitempty"`  // what couldn't be read
}

// BackupView is the Backups page.
type BackupView struct {
	Dir        string       `json:"dir"`
	Keep       int          `json:"keep"`
	Ready      bool         `json:"ready"`                // a passphrase is set
	DirProblem string       `json:"dirProblem,omitempty"` // why backups can't be saved there now
	Files      []BackupFile `json:"files"`
	Last       *JobResult   `json:"last,omitempty"`
}

func (a *Agent) backupView(ctx context.Context) BackupView {
	s := a.loadBackupSettings()
	v := BackupView{Dir: s.Dir, Keep: s.Keep, Ready: s.Key != nil, Files: a.listBackups(a.backupDirs(s.Dir)...), Last: a.jobs.lastResult("backup")}
	v.DirProblem = a.backupDirProblem(ctx, s.Dir)
	return v
}

// backupDirProblem says why backups can't be kept in dir: it has to be off
// the boot drive, or it's lost with it.
func (a *Agent) backupDirProblem(ctx context.Context, dir string) string {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return "Use a full path to a folder, like " + a.defaultBackupDir() + "."
	}
	// findmnt needs a path that exists, and the folder is only made by the
	// first backup: ask about the nearest one that does (the shares), which
	// is where the folder will be made.
	p := dir
	for p != "/" {
		if _, err := os.Stat(a.sys(p)); err == nil {
			break
		}
		p = filepath.Dir(p)
	}
	root, err1 := a.cmd(ctx, 5*time.Second, "findmnt", "-rno", "SOURCE", "--target", "/")
	here, err2 := a.cmd(ctx, 5*time.Second, "findmnt", "-rno", "SOURCE", "--target", p)
	if err1 != nil || err2 != nil {
		return "Couldn't tell which disk " + dir + " is on."
	}
	if strings.TrimSpace(root) == strings.TrimSpace(here) {
		return dir + " is on the boot drive right now (is the array stopped?), so a backup there would be lost with it."
	}
	return ""
}

// backupDirs are where backups can be: the folder, and for one in the
// shares the same folder on the cache and on each data disk, so they're
// found on a fresh install before the shares are mounted again.
func (a *Agent) backupDirs(dir string) []string {
	l := a.layout()
	dirs := []string{dir}
	rest, ok := strings.CutPrefix(dir, l.Shares+"/")
	if !ok {
		return dirs
	}
	dirs = append(dirs, filepath.Join(l.Cache, rest))
	for _, d := range a.dataDiskPaths(l) {
		dirs = append(dirs, filepath.Join(d, rest))
	}
	return dirs
}

// listBackups lists the backups in dirs, newest first; one that's in two
// of them (the folder through the shares and on its disk) counts once.
func (a *Agent) listBackups(dirs ...string) []BackupFile {
	l := a.layout()
	out := []BackupFile{}
	seen := map[string]bool{}
	for _, dir := range dirs {
		ents, err := os.ReadDir(a.sys(dir))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.Type().IsRegular() || !reBackupName.MatchString(e.Name()) || seen[e.Name()] {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			seen[e.Name()] = true
			out = append(out, BackupFile{Name: e.Name(), Size: fi.Size(), Created: fi.ModTime(), Dir: dir, On: l.where(dir)})
		}
	}
	// Newest first, by the time in the name: not by the whole name, or a
	// change of host name (a reinstall) would make pruning drop the newest.
	stamp := func(n string) string { return n[len(n)-len("20060102-150405.hsbak"):] }
	sort.Slice(out, func(i, j int) bool { return stamp(out[i].Name) > stamp(out[j].Name) })
	return out
}

// ---------- Making one ----------

// makeBackup saves a backup and drops the oldest beyond Keep.
func (a *Agent) makeBackup(ctx context.Context) (BackupFile, error) {
	s := a.loadBackupSettings()
	if s.Key == nil {
		return BackupFile{}, errors.New("set a backup passphrase first (Settings → Backups)")
	}
	if p := a.backupDirProblem(ctx, s.Dir); p != "" {
		return BackupFile{}, errors.New(p)
	}
	m, entries := a.collectBackup(ctx)
	plain, err := backup.Pack(m, entries)
	if err != nil {
		return BackupFile{}, err
	}
	sealed, err := backup.Seal(*s.Key, plain)
	if err != nil {
		return BackupFile{}, err
	}
	if len(sealed) > backup.MaxSize {
		return BackupFile{}, fmt.Errorf("the backup came to %s, more than the %s a backup can be", human(uint64(len(sealed))), human(backup.MaxSize))
	}
	dir := a.sys(s.Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BackupFile{}, err
	}
	name := fmt.Sprintf("homestack-backup-%s-%s.hsbak", m.Host, m.Created.Format("20060102-150405"))
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return BackupFile{}, err
	}
	if _, err := f.Write(sealed); err != nil {
		f.Close()
		os.Remove(tmp)
		return BackupFile{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return BackupFile{}, err
	}
	f.Close()
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return BackupFile{}, err
	}
	all := a.listBackups(s.Dir)
	for _, old := range all[min(len(all), s.Keep):] {
		_ = os.Remove(filepath.Join(dir, old.Name))
	}
	bf := BackupFile{Name: name, Size: int64(len(sealed)), Created: m.Created, Missing: m.Missing}
	for _, p := range m.Parts {
		info, _ := backup.Info(p.ID)
		c := lowerFirst(info.Label)
		if p.Note != "" {
			c += " (" + p.Note + ")"
		}
		bf.Contents = append(bf.Contents, c)
	}
	return bf, nil
}

// lowerFirst makes a label part of a sentence (but leaves names, like
// "UPS" and "Homestack's", alone).
func lowerFirst(s string) string {
	if strings.HasPrefix(s, "Homestack") {
		return s
	}
	if len(s) > 1 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'a' && s[1] <= 'z' {
		return strings.ToLower(s[:1]) + s[1:]
	}
	return s
}

// backupNow makes a backup and records how it went (Schedules shows it);
// a failure is also an alert, since an unnoticed one is how there's no
// backup the day it's needed.
func (a *Agent) backupNow(ctx context.Context) (BackupFile, error) {
	f, err := a.makeBackup(ctx)
	res := JobResult{Finished: time.Now(), OK: err == nil}
	if err != nil {
		res.Summary = "Backup failed: " + err.Error()
		a.notifyHost(notify.Error, "Backup failed", err.Error())
	} else {
		// What's in it, so a short one can be told from an incomplete one.
		res.Summary = fmt.Sprintf("Saved %s (%s): %s.", f.Name, human(uint64(f.Size)), strings.Join(f.Contents, ", "))
		if len(f.Missing) > 0 {
			res.Summary += " Couldn't read: " + strings.Join(f.Missing, "; ") + "."
			a.notifyHost(notify.Warning, "Backup incomplete", "Saved, but without: "+strings.Join(f.Missing, "; ")+".")
		}
	}
	b, _ := json.Marshal(res)
	_ = os.WriteFile(filepath.Join(a.opt.StateDir, "last-backup.json"), b, 0o644)
	return f, err
}

func hostName() string {
	h, _ := os.Hostname()
	h = regexp.MustCompile(`[^A-Za-z0-9.-]+`).ReplaceAllString(h, "-")
	if h = strings.Trim(h, ".-"); h == "" {
		h = "server"
	}
	return h
}

// shareUser is a Samba user's Linux account, so it can be made again with
// the same IDs (the files on the array are owned by those numbers).
type shareUser struct {
	Name  string `json:"name"`
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	Group string `json:"group"`
	Home  string `json:"home"`
	Shell string `json:"shell"`
}

// poolID is a ZFS pool, by the ID it's imported by after a reinstall (a
// name can be on two disks, like a stale label on an old parity disk).
type poolID struct {
	Name string `json:"name"`
	GUID string `json:"guid"`
}

// collectBackup reads everything a backup holds, part by part.
func (a *Agent) collectBackup(ctx context.Context) (backup.Manifest, []backup.Entry) {
	m := backup.Manifest{Format: 1, Created: time.Now(), Host: hostName(), Version: a.opt.Version}
	var entries []backup.Entry
	parts := map[string]*backup.Part{}
	add := func(part, name string, data []byte, mode os.FileMode) {
		entries = append(entries, backup.Entry{Name: name, Mode: int64(mode.Perm()), Data: data})
		if parts[part] == nil {
			parts[part] = &backup.Part{ID: part}
		}
		parts[part].Files = append(parts[part].Files, name)
	}
	file := func(part, name, src string) {
		b, err := os.ReadFile(src)
		switch {
		case err == nil:
			fi, _ := os.Stat(src)
			add(part, name, b, fi.Mode())
		case !errors.Is(err, os.ErrNotExist):
			m.Missing = append(m.Missing, fmt.Sprintf("%s: %v", src, err))
		}
	}
	jsonFile := func(part, name string, v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		add(part, name, b, 0o600)
	}
	state := func(n string) string { return filepath.Join(a.opt.StateDir, n) }

	// Homestack's own settings, without the sessions (useless after a
	// restore, and one secret fewer in the file).
	if a.opt.DataDir != "" {
		src := filepath.Join(a.opt.DataDir, "homestack.json")
		b, err := os.ReadFile(src)
		var st map[string]json.RawMessage
		if err == nil && json.Unmarshal(b, &st) == nil {
			delete(st, "sessions")
			jsonFile("homestack", "homestack/homestack.json", st)
		} else {
			m.Missing = append(m.Missing, fmt.Sprintf("Homestack's settings (%s): couldn't be read", src))
		}
	}

	// The array.
	jsonFile("storage", "storage/layout.json", a.layout())
	file("storage", "storage/snapraid.conf", a.snapraidConf())
	if v, err := a.cmd(ctx, 5*time.Second, "snapraid", "--version"); err == nil {
		// A restore checks the new server's isn't older (snapraidVersionWarning).
		add("storage", "storage/snapraid-version", []byte(strings.TrimSpace(strings.SplitN(v, "\n", 2)[0])+"\n"), 0o644)
	}
	file("storage", "storage/fstab", a.fstabPath())
	file("storage", "storage/zfs.conf", a.sys(modprobeFile))
	file("storage", "storage/array-disks.json", state(knownName))
	if names, err := a.cmd(ctx, 10*time.Second, "zpool", "list", "-Ho", "name"); err == nil {
		var pools []poolID
		for n := range strings.FieldsSeq(names) {
			if g, err := a.cmd(ctx, 10*time.Second, "zpool", "get", "-Ho", "value", "guid", n); err == nil {
				pools = append(pools, poolID{Name: n, GUID: strings.TrimSpace(g)})
			}
		}
		if len(pools) > 0 {
			jsonFile("storage", "storage/pools.json", pools)
			parts["storage"].Note = fmt.Sprintf("%d pool%s", len(pools), plural(len(pools)))
		}
	}

	// Docker's settings, and the boot-order drop-ins (Docker and libvirt wait
	// for the array, so nothing starts against empty folders).
	file("system", "system/daemon.json", a.sys(dockerDaemonJSON))
	dropins, _ := filepath.Glob(a.sys("/etc/systemd/system/*.service.d/" + waitDropIn))
	for _, d := range dropins {
		svc := filepath.Base(filepath.Dir(d))
		if reUnitDir.MatchString(svc) {
			file("system", "system/"+svc+"/"+waitDropIn, d)
		}
	}

	// Shares and their users.
	file("shares", "shares/smb.conf", a.smbConf())
	file("shares", "shares/homestack-shares.conf", a.smbInclude())
	file("shares", "shares/passdb.tdb", a.sys(passdbPath))
	file("shares", "shares/shares.json", a.sharesPath())
	file("shares", "shares/smb-users.json", state("smb-users.json"))
	if out, err := a.cmd(ctx, 10*time.Second, "pdbedit", "-L"); err == nil {
		var users []shareUser
		for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			name := strings.SplitN(l, ":", 2)[0]
			if name == "" {
				continue
			}
			if u, ok := a.lookupUser(ctx, name); ok {
				users = append(users, u)
			}
		}
		if len(users) > 0 {
			jsonFile("shares", "shares/users.json", users)
			parts["shares"].Note = fmt.Sprintf("%d share user%s", len(users), plural(len(users)))
		}
	}

	// Schedules and alerts, the mover's settings, where backups go (not the
	// key: the restore sets it from the passphrase).
	file("schedules", "schedules/homestack-alerts.cron", a.cronFile())
	file("schedules", "schedules/homestack-alerts", a.alertsConf())
	file("schedules", "schedules/schedules.json", a.schedulesPath())
	file("schedules", "schedules/mover.json", state("mover.json"))
	file("schedules", "schedules/parity.json", state("parity.json"))
	file("schedules", "schedules/spindown.json", state("spindown.json"))
	s := a.loadBackupSettings()
	jsonFile("schedules", "schedules/backup.json", backupSettings{Dir: s.Dir, Keep: s.Keep})

	file("fans", "fans/fans.json", state("fans.json"))
	file("fans", "fans/homestack-fans.conf", a.sys(fansModules))

	file("ups", "ups/homestack-ups", a.sys(upsDefaults))
	for _, n := range nutFiles {
		file("ups", "ups/nut/"+n, a.sys("/etc/nut/"+n))
	}

	// VMs: their definitions, UEFI variables, and which start with the server.
	if a.have(ctx, "virsh") {
		names, _ := a.cmd(ctx, 20*time.Second, "virsh", "list", "--all", "--name")
		auto, _ := a.cmd(ctx, 20*time.Second, "virsh", "list", "--all", "--autostart", "--name")
		var vms, autostart []string
		for n := range strings.FieldsSeq(names) {
			if !reVMName.MatchString(n) {
				m.Missing = append(m.Missing, fmt.Sprintf("the VM %q (its name can't be a file name)", n))
				continue
			}
			x, err := a.cmd(ctx, 20*time.Second, "virsh", "dumpxml", "--inactive", "--security-info", n)
			if err != nil {
				m.Missing = append(m.Missing, fmt.Sprintf("the VM %s: %s", n, strings.TrimSpace(x)))
				continue
			}
			add("vms", "vms/"+n+".xml", []byte(x), 0o600)
			if nv := nvramOf(x); nv != "" {
				file("vms", "vms/"+n+".nvram", a.sys(nv))
			}
			vms = append(vms, n)
			if slices.Contains(strings.Fields(auto), n) {
				autostart = append(autostart, n)
			}
		}
		if len(vms) > 0 {
			jsonFile("vms", "vms/autostart.json", autostart)
			parts["vms"].Note = fmt.Sprintf("%d VM%s", len(vms), plural(len(vms)))
		}
	}

	if ents, err := os.ReadDir(a.opt.NetDir); err == nil {
		for _, e := range ents {
			if e.Type().IsRegular() && reNetFile.MatchString(e.Name()) {
				file("network", "network/"+e.Name(), filepath.Join(a.opt.NetDir, e.Name()))
			}
		}
	}

	for _, p := range backup.Parts {
		if parts[p.ID] != nil {
			m.Parts = append(m.Parts, *parts[p.ID])
		}
	}
	return m, entries
}

var (
	reVMName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reNVRAM  = regexp.MustCompile(`<nvram[^>]*>([^<]+)</nvram>`)
)

// nvramOf is a VM's UEFI variables file, if it's where libvirt keeps them.
func nvramOf(xml string) string {
	m := reNVRAM.FindStringSubmatch(xml)
	if m == nil {
		return ""
	}
	p := filepath.Clean(strings.TrimSpace(m[1]))
	if !strings.HasPrefix(p, nvramDir) || strings.Contains(strings.TrimPrefix(p, nvramDir), "/") {
		return ""
	}
	return p
}

func (a *Agent) lookupUser(ctx context.Context, name string) (shareUser, bool) {
	out, err := a.cmd(ctx, 5*time.Second, "getent", "passwd", name)
	f := strings.Split(strings.TrimSpace(out), ":")
	if err != nil || len(f) < 7 {
		return shareUser{}, false
	}
	u := shareUser{Name: f[0], Home: f[5], Shell: f[6]}
	u.UID, _ = strconv.Atoi(f[2])
	u.GID, _ = strconv.Atoi(f[3])
	if g, err := a.cmd(ctx, 5*time.Second, "getent", "group", f[3]); err == nil {
		u.Group = strings.SplitN(strings.TrimSpace(g), ":", 2)[0]
	}
	return u, true
}

// ---------- Restoring ----------

// RestoreResult is how restoring one part went.
type RestoreResult struct {
	Part   string   `json:"part"`
	OK     bool     `json:"ok"`
	Detail string   `json:"detail"`
	Saved  []string `json:"saved,omitempty"` // the files it replaced, kept next to them
	Warn   string   `json:"warn,omitempty"`  // done, but something needs doing by hand
}

// restoreBackup restores the server's parts picked from an opened backup,
// in order (the array first: shares and VMs live on it).
func (a *Agent) restoreBackup(ctx context.Context, files map[string]backup.Entry, picked []string) []RestoreResult {
	stamp := time.Now().Format("20060102-150405")
	var out []RestoreResult
	for _, p := range backup.Parts {
		if !p.Host || !slices.Contains(picked, p.ID) {
			continue
		}
		r := RestoreResult{Part: p.ID}
		g := &fileGuard{Suffix: "restore-" + stamp, Saved: func(b string) { r.Saved = append(r.Saved, b) }}
		detail, err := a.restorePart(ctx, p.ID, files, g)
		r.OK, r.Detail = err == nil, detail
		if err != nil {
			r.Detail = err.Error()
		}
		if p.ID == "storage" {
			r.Warn = a.snapraidVersionWarning(ctx, files)
		}
		out = append(out, r)
	}
	return out
}

func (a *Agent) restorePart(ctx context.Context, id string, files map[string]backup.Entry, g *fileGuard) (string, error) {
	has := func(n string) (backup.Entry, bool) { e, ok := files[n]; return e, ok }
	put := func(n, dest string) (bool, error) {
		e, ok := has(n)
		if !ok {
			return false, nil
		}
		return true, a.writeRestored(g, dest, e.Data, os.FileMode(e.Mode))
	}
	state := func(n string) string { return filepath.Join(a.opt.StateDir, n) }
	var done []string
	switch id {
	case "storage":
		// Where everything is mounted first: snapraid.conf's place and the
		// mounts below depend on it.
		if e, ok := has("storage/layout.json"); ok {
			var l Layout
			if err := json.Unmarshal(e.Data, &l); err != nil {
				return "", errors.New("the storage layout in the backup doesn't read")
			}
			if err := l.validate(); err != nil {
				return "", fmt.Errorf("the storage layout in the backup isn't usable: %v", err)
			}
			b, _ := json.MarshalIndent(l, "", "  ")
			if err := a.writeRestored(g, a.layoutPath(), b, 0o644); err != nil {
				return "", err
			}
			if err := a.useLayout(l); err != nil {
				return "", err
			}
			done = append(done, "the storage layout")
		}
		if e, ok := has("storage/snapraid.conf"); ok {
			if err := checkSnapConf(e.Data); err != nil {
				return "", err
			}
			if err := a.writeRestored(g, a.snapraidConf(), e.Data, os.FileMode(e.Mode)); err != nil {
				return "", err
			}
			done = append(done, "snapraid.conf")
		}
		if ok, err := put("storage/zfs.conf", a.sys(modprobeFile)); err != nil {
			return "", err
		} else if ok {
			done = append(done, "the ZFS cache limit (from the next restart)")
		}
		if _, err := put("storage/array-disks.json", state(knownName)); err != nil {
			return "", err
		}
		// The pools, by ID.
		if e, ok := has("storage/pools.json"); ok {
			var pools []poolID
			_ = json.Unmarshal(e.Data, &pools)
			have := map[string]bool{}
			for _, p := range a.pools(ctx) {
				have[p.Name] = true
			}
			var imported, failed []string
			for _, p := range pools {
				if have[p.Name] || !regexp.MustCompile(`^\d+$`).MatchString(p.GUID) {
					continue
				}
				if out, err := a.cmd(ctx, 5*time.Minute, "zpool", "import", p.GUID); err != nil {
					failed = append(failed, fmt.Sprintf("%s (%s)", p.Name, lastLine(out)))
				} else {
					imported = append(imported, p.Name)
				}
			}
			if len(imported) > 0 {
				done = append(done, "imported "+strings.Join(imported, ", "))
			}
			if len(failed) > 0 {
				done = append(done, "couldn't import "+strings.Join(failed, ", "))
			}
		}
		// The array's mounts: only the storage's (Layout.ownsMount), and only
		// those missing. The rest of fstab is this install's own (its boot
		// drive's UUIDs).
		lay := a.layout()
		if e, ok := has("storage/fstab"); ok {
			var added []string
			err := a.editFstabG(ctx, g, func(lines []string) ([]string, error) {
				cur := map[string]bool{}
				for _, l := range lines {
					if f := fstabFields(l); f != nil {
						cur[f[1]] = true
					}
				}
				for l := range strings.SplitSeq(string(e.Data), "\n") {
					if f := fstabFields(l); f != nil && lay.ownsMount(f[1]) && !cur[f[1]] {
						if len(added) == 0 {
							lines = append(lines, "", "# The array's mounts, restored by Homestack on "+time.Now().Format("2 Jan 2006"))
						}
						lines = append(lines, l)
						added = append(added, f[1])
						cur[f[1]] = true
					}
				}
				if len(added) == 0 {
					return nil, nil
				}
				return lines, nil
			})
			if err != nil {
				return "", err
			}
			if len(added) > 0 {
				for _, m := range added {
					_ = os.MkdirAll(a.sys(m), 0o755)
				}
				out, err := a.cmd(ctx, 5*time.Minute, "mount", "-a")
				done = append(done, fmt.Sprintf("added %s to /etc/fstab", strings.Join(added, ", ")))
				if err != nil {
					done = append(done, "mount -a: "+lastLine(out))
				}
			}
		}
	case "system":
		if e, ok := has("system/daemon.json"); ok {
			var x map[string]any
			if json.Unmarshal(e.Data, &x) != nil {
				return "", errors.New("the backup's /etc/docker/daemon.json isn't valid JSON, so it wasn't used")
			}
			if err := a.writeRestored(g, a.sys(dockerDaemonJSON), e.Data, 0o644); err != nil {
				return "", err
			}
			done = append(done, "Docker's settings (from Docker's next restart)")
		}
		var units []string
		for name, e := range files {
			rest, ok := strings.CutPrefix(name, "system/")
			svc, file, ok2 := strings.Cut(rest, "/")
			if !ok || !ok2 || file != waitDropIn || !reUnitDir.MatchString(svc) {
				continue
			}
			if err := a.writeRestored(g, a.sys("/etc/systemd/system/"+svc+"/"+waitDropIn), e.Data, 0o644); err != nil {
				return "", err
			}
			units = append(units, strings.TrimSuffix(svc, ".service.d"))
		}
		if len(units) > 0 {
			sort.Strings(units)
			_, _ = a.cmd(ctx, time.Minute, "systemctl", "daemon-reload")
			done = append(done, strings.Join(units, ", ")+" wait for the array at boot")
		}
	case "shares":
		if e, ok := has("shares/users.json"); ok {
			var users []shareUser
			_ = json.Unmarshal(e.Data, &users)
			for _, u := range users {
				if s := a.restoreUser(ctx, u); s != "" {
					done = append(done, s)
				}
			}
		}
		smbOld, smbHad := os.ReadFile(a.smbConf())
		incOld, incHad := os.ReadFile(a.smbInclude())
		okConf, err := put("shares/smb.conf", a.smbConf())
		if err != nil {
			return "", err
		}
		okInc, err := put("shares/homestack-shares.conf", a.smbInclude())
		if err != nil {
			return "", err
		}
		if okConf || okInc {
			if out, err := a.cmd(ctx, 30*time.Second, "testparm", "-s", a.smbConf()); err != nil {
				// Put Samba's files back as they were: a broken smb.conf
				// would stop every share.
				for _, f := range []struct {
					path string
					old  []byte
					had  error
				}{{a.smbConf(), smbOld, smbHad}, {a.smbInclude(), incOld, incHad}} {
					if f.had == nil {
						_ = os.WriteFile(f.path, f.old, 0o644)
					} else {
						_ = os.Remove(f.path)
					}
				}
				return "", fmt.Errorf("the backup's Samba settings didn't pass testparm (%s), so they weren't used", lastLine(out))
			}
			done = append(done, "Samba's settings")
		}
		if e, ok := has("shares/passdb.tdb"); ok {
			if a.have(ctx, "smbd") {
				_, _ = a.cmd(ctx, time.Minute, "systemctl", "stop", "smbd")
				err := a.writeRestored(g, a.sys(passdbPath), e.Data, 0o600)
				_, _ = a.cmd(ctx, time.Minute, "systemctl", "start", "smbd")
				if err != nil {
					return "", err
				}
				done = append(done, "the share users' passwords")
			} else {
				done = append(done, "not the passwords: Samba isn't installed (apt install samba, then restore Shares again)")
			}
		}
		for _, n := range []string{"shares.json", "smb-users.json"} {
			if _, err := put("shares/"+n, state(n)); err != nil {
				return "", err
			}
		}
		_, _ = a.cmd(ctx, 30*time.Second, "systemctl", "reload-or-restart", "smbd")
	case "schedules":
		for _, f := range [][2]string{{"schedules/homestack-alerts.cron", a.cronFile()}, {"schedules/homestack-alerts", a.alertsConf()},
			{"schedules/schedules.json", a.schedulesPath()}, {"schedules/mover.json", state("mover.json")},
			{"schedules/parity.json", a.parityPath()}, {"schedules/spindown.json", a.spinPath()}} {
			if ok, err := put(f[0], f[1]); err != nil {
				return "", err
			} else if ok {
				done = append(done, filepath.Base(f[1]))
			}
		}
		if _, ok := has("schedules/parity.json"); ok {
			if err := a.writeParityFile(a.loadParity()); err != nil {
				return "", err
			}
		}
		if e, ok := has("schedules/backup.json"); ok {
			var b backupSettings
			if json.Unmarshal(e.Data, &b) == nil && b.Dir != "" {
				cur := a.loadBackupSettings()
				cur.Dir, cur.Keep = b.Dir, max(b.Keep, 1)
				if err := a.saveBackupSettings(cur); err != nil {
					return "", err
				}
				done = append(done, "the backups folder")
			}
		}
	case "fans":
		if _, err := put("fans/fans.json", state("fans.json")); err != nil {
			return "", err
		}
		if ok, err := put("fans/homestack-fans.conf", a.sys(fansModules)); err != nil {
			return "", err
		} else if ok {
			for mod := range strings.FieldsSeq(string(files["fans/homestack-fans.conf"].Data)) {
				_, _ = a.cmd(ctx, 30*time.Second, "modprobe", mod)
			}
		}
		a.fans.mu.Lock()
		a.fans.loadConfig()
		a.fans.mu.Unlock()
		done = append(done, "the fan curves")
	case "ups":
		if _, err := put("ups/homestack-ups", a.sys(upsDefaults)); err != nil {
			return "", err
		}
		if _, err := os.Stat(a.sys("/etc/nut")); err != nil {
			return "Saved the UPS settings. NUT isn't installed: run deploy/debian/ups/install.sh, which uses them.", nil
		}
		for _, n := range nutFiles {
			if ok, err := put("ups/nut/"+n, a.sys("/etc/nut/"+n)); err != nil {
				return "", err
			} else if ok {
				_, _ = a.cmd(ctx, 10*time.Second, "chown", "root:nut", "/etc/nut/"+n)
			}
		}
		_, _ = a.cmd(ctx, time.Minute, "systemctl", "restart", "nut-server", "nut-monitor")
		done = append(done, "the UPS settings")
	case "vms":
		if !a.have(ctx, "virsh") {
			return "", errors.New("libvirt isn't installed (apt install libvirt-daemon-system), so the VMs can't be defined yet. Restore this part again once it is")
		}
		existing, _ := a.cmd(ctx, 20*time.Second, "virsh", "list", "--all", "--name")
		var autostart []string
		if e, ok := has("vms/autostart.json"); ok {
			_ = json.Unmarshal(e.Data, &autostart)
		}
		var defined, skipped []string
		for name, e := range files {
			n, ok := strings.CutPrefix(name, "vms/")
			if !ok || !strings.HasSuffix(n, ".xml") {
				continue
			}
			vm := strings.TrimSuffix(n, ".xml")
			if !reVMName.MatchString(vm) {
				continue
			}
			if slices.Contains(strings.Fields(existing), vm) {
				skipped = append(skipped, vm)
				continue
			}
			if nv := nvramOf(string(e.Data)); nv != "" {
				if v, ok := has("vms/" + vm + ".nvram"); ok {
					if err := a.writeRestored(g, a.sys(nv), v.Data, 0o600); err != nil {
						return "", err
					}
				}
			}
			tmp, err := os.CreateTemp(a.opt.StateDir, "restore-vm-*.xml")
			if err != nil {
				return "", err
			}
			tmp.Write(e.Data)
			tmp.Close()
			out, err := a.cmd(ctx, time.Minute, "virsh", "define", tmp.Name())
			os.Remove(tmp.Name())
			if err != nil {
				return "", fmt.Errorf("virsh define %s: %s", vm, lastLine(out))
			}
			if slices.Contains(autostart, vm) {
				_, _ = a.cmd(ctx, 20*time.Second, "virsh", "autostart", vm)
			}
			defined = append(defined, vm)
		}
		sort.Strings(defined)
		if len(defined) > 0 {
			done = append(done, "defined "+strings.Join(defined, ", "))
		}
		if len(skipped) > 0 {
			sort.Strings(skipped)
			done = append(done, "left "+strings.Join(skipped, ", ")+" as they are (already defined)")
		}
	case "network":
		var wrote []string
		for name, e := range files {
			n, ok := strings.CutPrefix(name, "network/")
			if !ok || !reNetFile.MatchString(n) {
				continue
			}
			if err := a.writeRestored(g, filepath.Join(a.opt.NetDir, n), e.Data, 0o644); err != nil {
				return "", err
			}
			wrote = append(wrote, n)
		}
		sort.Strings(wrote)
		if len(wrote) > 0 {
			done = append(done, fmt.Sprintf("%d file%s in %s; they take effect at the next restart (or sudo networkctl reload)", len(wrote), plural(len(wrote)), a.opt.NetDir))
		}
	default:
		return "", fmt.Errorf("unknown part %q", id)
	}
	if len(done) == 0 {
		return "Nothing of this part was in the backup.", nil
	}
	return strings.Join(done, "; ") + ".", nil
}

// writeRestored writes a restored file atomically, keeping the one it
// replaces next to it (unless they're the same).
func (a *Agent) writeRestored(g *fileGuard, dest string, data []byte, mode os.FileMode) error {
	if mode.Perm() == 0 {
		mode = 0o600
	}
	if cur, err := os.ReadFile(dest); err == nil {
		if string(cur) == string(data) {
			return nil
		}
		if err := a.backupOnce(g, dest); err != nil {
			return err
		}
		if fi, err := os.Stat(dest); err == nil {
			mode = fi.Mode().Perm() // keep the permissions it has here
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".homestack-tmp")
	if err := os.WriteFile(tmp, data, mode.Perm()); err != nil {
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		f.Close()
	}
	return os.Rename(tmp, dest)
}

var reSnapVersion = regexp.MustCompile(`\bv(\d+)\.(\d+)`)

// snapraidVersionWarning says when this server's SnapRAID is older than the
// one the backup was made with (or missing). An older one may not read the
// content file a newer one wrote, or newer options in snapraid.conf (like
// 15.0's snapshot), so parity syncs would fail until it's updated.
func (a *Agent) snapraidVersionWarning(ctx context.Context, files map[string]backup.Entry) string {
	e, ok := files["storage/snapraid-version"]
	if !ok {
		return ""
	}
	want := reSnapVersion.FindStringSubmatch(string(e.Data))
	if want == nil {
		return ""
	}
	wantV := want[1] + "." + want[2]
	o, err := a.cmd(ctx, 5*time.Second, "snapraid", "--version")
	if err != nil {
		return fmt.Sprintf("SnapRAID isn't installed. Install %s or newer (the version the backup was made with) before the next parity sync.", wantV)
	}
	have := reSnapVersion.FindStringSubmatch(o)
	if have == nil {
		return ""
	}
	num := func(m []string) (int, int) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		return x, y
	}
	hx, hy := num(have)
	wx, wy := num(want)
	if hx > wx || hx == wx && hy >= wy {
		return ""
	}
	return fmt.Sprintf("This server has SnapRAID %s.%s, older than the %s the backup was made with. Install %s or newer before the next parity sync: an older one may not read the content file or the options in snapraid.conf.", have[1], have[2], wantV, wantV)
}

// checkSnapConf makes sure a snapraid.conf reads, and lists data disks.
func checkSnapConf(b []byte) error {
	f, err := os.CreateTemp("", "snapraid-*.conf")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	f.Write(b)
	f.Close()
	c, err := readSnapConf(f.Name())
	if err != nil || len(c.data) == 0 {
		return errors.New("the backup's snapraid.conf has no data disks in it, so it wasn't used")
	}
	return nil
}

// restoreUser makes a share user's account again, with its old IDs.
func (a *Agent) restoreUser(ctx context.Context, u shareUser) string {
	if u.UID < 1000 || u.UID >= 60000 { // a system account Samba knows (nobody…): this install has its own
		return ""
	}
	if !reUser.MatchString(u.Name) || u.GID < 100 {
		return fmt.Sprintf("skipped the user %q (not a usual account name or group)", u.Name)
	}
	if cur, ok := a.lookupUser(ctx, u.Name); ok {
		if cur.UID != u.UID {
			return fmt.Sprintf("%s is there already, but as user %d, not %d: files on the array may show another owner", u.Name, cur.UID, u.UID)
		}
		return u.Name + " is there already"
	}
	group := strconv.Itoa(u.GID)
	if out, err := a.cmd(ctx, 5*time.Second, "getent", "group", group); err != nil || strings.TrimSpace(out) == "" {
		name := u.Group
		if name == "" || !reUser.MatchString(name) {
			name = u.Name
		}
		if out, err := a.cmd(ctx, 10*time.Second, "groupadd", "-g", group, name); err != nil {
			return fmt.Sprintf("couldn't make %s's group %d (%s)", u.Name, u.GID, lastLine(out))
		}
	}
	args := []string{"-M", "-N", "-g", group, "-s", "/usr/sbin/nologin"}
	if u.Home != "" && strings.HasPrefix(u.Home, "/") {
		args = append(args, "-d", u.Home)
	}
	uid := strconv.Itoa(u.UID)
	if out, err := a.cmd(ctx, 5*time.Second, "getent", "passwd", uid); err == nil && strings.TrimSpace(out) != "" {
		if out, err := a.cmd(ctx, 10*time.Second, "useradd", append(args, u.Name)...); err != nil {
			return fmt.Sprintf("couldn't make %s (%s)", u.Name, lastLine(out))
		}
		return fmt.Sprintf("made %s, but not as user %d (that's taken): files on the array may show another owner", u.Name, u.UID)
	}
	if out, err := a.cmd(ctx, 10*time.Second, "useradd", append(append(args, "-u", uid), u.Name)...); err != nil {
		return fmt.Sprintf("couldn't make %s (%s)", u.Name, lastLine(out))
	}
	return fmt.Sprintf("made %s (user %d)", u.Name, u.UID)
}

var reUser = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)

// ---------- API ----------

func (a *Agent) getBackups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.backupView(r.Context()))
}

func (a *Agent) putBackupSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir        *string `json:"dir"`
		Keep       *int    `json:"keep"`
		Passphrase *string `json:"passphrase"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	s := a.loadBackupSettings()
	if req.Dir != nil {
		d := strings.TrimRight(strings.TrimSpace(*req.Dir), "/")
		if p := a.backupDirProblem(r.Context(), d); p != "" {
			writeErr(w, 409, p)
			return
		}
		s.Dir = d
	}
	if req.Keep != nil {
		if *req.Keep < 1 || *req.Keep > 365 {
			writeErr(w, 409, "Keep between 1 and 365 backups.")
			return
		}
		s.Keep = *req.Keep
	}
	if req.Passphrase != nil {
		k, err := backup.NewKey(*req.Passphrase)
		if err != nil {
			writeErr(w, 409, strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
			return
		}
		s.Key = &k
	}
	if err := a.saveBackupSettings(s); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, a.backupView(r.Context()))
}

func (a *Agent) postBackup(w http.ResponseWriter, r *http.Request) {
	f, err := a.backupNow(r.Context())
	if err != nil {
		writeErr(w, 409, strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
		return
	}
	writeJSON(w, 200, map[string]any{"file": f, "view": a.backupView(r.Context())})
}

func (a *Agent) getBackupFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !reBackupName.MatchString(name) {
		writeErr(w, 404, "There's no such backup.")
		return
	}
	var b []byte
	err := os.ErrNotExist
	for _, f := range a.listBackups(a.backupDirs(a.loadBackupSettings().Dir)...) {
		if f.Name == name {
			b, err = os.ReadFile(filepath.Join(a.sys(f.Dir), name))
			break
		}
	}
	if err != nil {
		writeErr(w, 404, "There's no such backup.")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

// postRestore restores the server's parts of a backup Homestack opened.
func (a *Agent) postRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Archive string   `json:"archive"` // the opened (decrypted) tar.gz
		Parts   []string `json:"parts"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6*backup.MaxSize))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Archive)
	if err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	_, files, err := backup.Unpack(raw)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	if op := a.opNow(); op.active() {
		writeErr(w, 409, opTitle(op)+" is in progress. Restore once it's finished.")
		return
	}
	if err := a.notBusy(r.Context()); err != nil { // not halfway through a sync or the mover
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, a.restoreBackup(r.Context(), files, req.Parts))
}
