package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"homestack/internal/fans"
)

// Samba shares, managed like Unraid's: each share is exported (or hidden),
// with a security level (public / secure / private) and per-user access.
// The agent owns /etc/samba/homestack-shares.conf and includes it from
// smb.conf; [global] stays hand-edited.

type Share struct {
	Name     string            `json:"name"`
	Path     string            `json:"path"`
	Comment  string            `json:"comment,omitempty"`
	Export   string            `json:"export"`   // yes, hidden, no
	Security string            `json:"security"` // public, secure, private
	Access   map[string]string `json:"access"`   // user -> "ro" | "rw"
	Mover    bool              `json:"mover"`    // move from the cache to the array nightly
}

type sharesState struct {
	Shares    []Share `json:"shares"`
	MoverInit bool    `json:"moverInit"` // the mover flags have been set up
}

func (a *Agent) smbConf() string {
	if v := os.Getenv("HOMESTACK_SMB_CONF"); v != "" {
		return v
	}
	return "/etc/samba/smb.conf"
}
func (a *Agent) smbInclude() string {
	return filepath.Join(filepath.Dir(a.smbConf()), "homestack-shares.conf")
}
func (a *Agent) sharesPath() string { return filepath.Join(a.opt.StateDir, "shares.json") }

// createdUsers are Linux accounts Homestack made for Samba users; they're
// removed again with the Samba user. Accounts that existed before are left.
func (a *Agent) createdUsers() []string {
	var u []string
	if b, err := os.ReadFile(filepath.Join(a.opt.StateDir, "smb-users.json")); err == nil {
		_ = json.Unmarshal(b, &u)
	}
	return u
}
func (a *Agent) saveCreatedUsers(u []string) {
	_ = fans.WriteJSON(filepath.Join(a.opt.StateDir, "smb-users.json"), u, 0o640)
}

func (a *Agent) loadShares() (sharesState, bool) {
	var st sharesState
	b, err := os.ReadFile(a.sharesPath())
	if err != nil {
		return st, false
	}
	if json.Unmarshal(b, &st) != nil {
		return st, false
	}
	if !st.MoverInit {
		// Shares taken over before the mover option existed: start from the
		// mover's current list, so nothing changes until you change it.
		cur := map[string]bool{}
		for _, f := range a.alertsMoverShares() {
			cur[f] = true
		}
		l := a.layout()
		for i := range st.Shares {
			if f := l.shareFolder(st.Shares[i].Path); f != "" && cur[f] {
				st.Shares[i].Mover = true
			}
		}
		st.MoverInit = true
	}
	return st, true
}

// ---------- The mover's share list ----------

func (a *Agent) alertsConf() string {
	if v := os.Getenv("HOMESTACK_ALERTS_CONF"); v != "" {
		return v
	}
	return "/etc/default/homestack-alerts"
}
func (a *Agent) moverSharesFile() string {
	return filepath.Join(filepath.Dir(a.alertsConf()), "homestack-mover-shares")
}

// alertsMoverShares is MOVER_SHARES from the alerts settings (the list set
// by hand before shares were managed here). None when it isn't set.
func (a *Agent) alertsMoverShares() []string {
	b, err := os.ReadFile(a.alertsConf())
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`(?m)^\s*MOVER_SHARES=["']?([^"'#\n]*)`)
	if m := re.FindSubmatch(b); m != nil {
		return strings.Fields(string(m[1]))
	}
	return nil
}

// writeMoverShares tells the mover which folders to move (see writeMoverFile).
func (a *Agent) writeMoverShares(st sharesState) error { return a.writeMoverFile(&st) }

// moverShareList is the folders the mover moves: shares with the option on,
// plus folders in the old MOVER_SHARES list that aren't a share (such as isos).
func (a *Agent) moverShareList(st sharesState) []string {
	l := a.layout()
	isShare := map[string]bool{}
	var list []string
	for _, s := range st.Shares {
		if f := l.shareFolder(s.Path); f != "" {
			isShare[f] = true
			if s.Mover {
				list = append(list, f)
			}
		}
	}
	for _, f := range a.alertsMoverShares() {
		if !isShare[f] && l.shareFolder(l.Shares+"/"+f) != "" {
			list = append(list, f)
		}
	}
	sort.Strings(list)
	return list
}

var (
	reShareName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	reUserName  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reserved    = map[string]bool{"global": true, "homes": true, "printers": true, "print$": true, "ipc$": true}
)

func (s *Share) validate(l Layout, users map[string]bool) error {
	s.Name = strings.TrimSpace(s.Name)
	if !reShareName.MatchString(s.Name) || reserved[strings.ToLower(s.Name)] {
		return fmt.Errorf("Share names can use letters, numbers, spaces, dots, - and _ (up to 64), and can't be %q.", "global/homes/printers")
	}
	s.Path = filepath.Clean(strings.TrimSpace(s.Path))
	if !filepath.IsAbs(s.Path) || !l.inStorage(s.Path) {
		return fmt.Errorf("The folder must be inside %s, %s, %s or a data disk (%sN).", l.Shares, l.Cache, l.Unassigned, l.DiskPrefix)
	}
	if strings.ContainsAny(s.Comment, "\n\r") || len(s.Comment) > 120 {
		return errors.New("Keep the description to one line (up to 120 characters).")
	}
	switch s.Export {
	case "yes", "hidden", "no":
	default:
		return errors.New("Unknown export setting.")
	}
	switch s.Security {
	case "public", "secure", "private":
	default:
		return errors.New("Unknown security setting.")
	}
	if s.Mover && l.shareFolder(s.Path) == "" {
		return fmt.Errorf("Moving to the array only works for shares in %s (one folder deep, no spaces or quotes in the folder name).", l.Shares)
	}
	clean := map[string]string{}
	for u, v := range s.Access {
		if !users[u] {
			continue // user no longer exists
		}
		if v == "ro" || v == "rw" {
			clean[u] = v
		}
	}
	s.Access = clean
	return nil
}

// render writes the managed include file's text.
func renderShares(list []Share) string {
	var b strings.Builder
	b.WriteString("# Managed by Homestack (Shares page). Changes made here are overwritten.\n")
	sorted := append([]Share(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name) })
	for _, s := range sorted {
		if s.Export == "no" {
			continue
		}
		var ro, rw []string
		for u, v := range s.Access {
			if v == "rw" {
				rw = append(rw, u)
			} else {
				ro = append(ro, u)
			}
		}
		sort.Strings(ro)
		sort.Strings(rw)
		fmt.Fprintf(&b, "\n[%s]\n   path = %s\n", s.Name, s.Path)
		if s.Comment != "" {
			fmt.Fprintf(&b, "   comment = %s\n", s.Comment)
		}
		fmt.Fprintf(&b, "   browseable = %s\n", map[bool]string{true: "yes", false: "no"}[s.Export == "yes"])
		switch s.Security {
		case "public":
			b.WriteString("   guest ok = yes\n   read only = no\n")
		case "secure":
			b.WriteString("   guest ok = yes\n   read only = yes\n")
			if len(rw) > 0 {
				fmt.Fprintf(&b, "   write list = %s\n", strings.Join(rw, " "))
			}
		case "private":
			all := append(append([]string{}, rw...), ro...)
			sort.Strings(all)
			if len(all) == 0 {
				b.WriteString("   # nobody has access yet\n   valid users = root\n   read only = yes\n")
				continue
			}
			fmt.Fprintf(&b, "   valid users = %s\n   read only = yes\n", strings.Join(all, " "))
			if len(rw) > 0 {
				fmt.Fprintf(&b, "   write list = %s\n", strings.Join(rw, " "))
			}
		}
	}
	return b.String()
}

// ---------- Samba users ----------

type SmbUser struct {
	Name    string `json:"name"`
	UID     int    `json:"uid"`
	Created bool   `json:"createdHere"`
}

func (a *Agent) smbUsers(ctx context.Context) ([]SmbUser, error) {
	out, err := a.cmd(ctx, 10*time.Second, "pdbedit", "-L")
	if err != nil {
		return nil, fmt.Errorf("pdbedit: %s", strings.TrimSpace(out))
	}
	created := map[string]bool{}
	for _, u := range a.createdUsers() {
		created[u] = true
	}
	var users []SmbUser
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		var uid int
		fmt.Sscan(f[1], &uid)
		users = append(users, SmbUser{Name: f[0], UID: uid, Created: created[f[0]]})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	return users, nil
}

func (a *Agent) cmdStdin(ctx context.Context, input, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ---------- Importing the shares already in smb.conf ----------

type confSection struct {
	name  string
	lines []string // raw lines, header included
}

func splitConf(text string) (head []string, sections []confSection) {
	re := regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)
	var cur *confSection
	for line := range strings.SplitSeq(text, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			sections = append(sections, confSection{name: strings.TrimSpace(m[1])})
			cur = &sections[len(sections)-1]
		}
		if cur == nil {
			head = append(head, line)
		} else {
			cur.lines = append(cur.lines, line)
		}
	}
	return
}

func parseParams(lines []string) map[string]string {
	p := map[string]string{}
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k = strings.Join(strings.Fields(strings.ToLower(k)), " ")
		p[k] = strings.TrimSpace(v)
	}
	return p
}

func yes(v string) bool {
	switch strings.ToLower(v) {
	case "yes", "true", "1":
		return true
	}
	return false
}

// shareFromParams maps smb.conf settings onto the Unraid-style model.
func shareFromParams(name string, p map[string]string, users []string) Share {
	s := Share{Name: name, Path: p["path"], Comment: p["comment"], Export: "yes", Access: map[string]string{}}
	if v, ok := p["browseable"]; ok && !yes(v) {
		s.Export = "hidden"
	}
	if v, ok := p["browsable"]; ok && !yes(v) {
		s.Export = "hidden"
	}
	readOnly := true
	if v, ok := p["read only"]; ok {
		readOnly = yes(v)
	}
	for _, k := range []string{"writeable", "writable", "write ok"} {
		if v, ok := p[k]; ok {
			readOnly = !yes(v)
		}
	}
	guest := yes(p["guest ok"]) || yes(p["public"])
	split := func(v string) []string {
		return strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' })
	}
	writeList := split(p["write list"])
	valid := split(p["valid users"])
	switch {
	case guest && !readOnly:
		s.Security = "public"
	case guest:
		s.Security = "secure"
		for _, u := range writeList {
			s.Access[u] = "rw"
		}
	default:
		s.Security = "private"
		if len(valid) == 0 {
			valid = users // any signed-in user
		}
		for _, u := range valid {
			if readOnly {
				s.Access[u] = "ro"
			} else {
				s.Access[u] = "rw"
			}
		}
		for _, u := range writeList {
			s.Access[u] = "rw"
		}
	}
	return s
}

var specialSections = map[string]bool{"global": true, "homes": true, "printers": true, "print$": true}

// ---------- API ----------

type sharesView struct {
	Managed  bool       `json:"managed"`
	Samba    bool       `json:"samba"`
	Shares   []Share    `json:"shares"`
	Existing []Share    `json:"existing,omitempty"` // shares found in smb.conf, before takeover
	Users    []SmbUser  `json:"users"`
	Conf     string     `json:"conf"`
	Error    string     `json:"error,omitempty"`
	Sessions []smbConn  `json:"sessions"`
	Missing  []string   `json:"missingFolders,omitempty"`
	Checked  *time.Time `json:"checked,omitempty"`
	Root     string     `json:"root"`  // the shares (Layout): a new share's folder goes here
	Owner    string     `json:"owner"` // who owns new share folders, for people
	Host     string     `json:"host"`  // the server's name, to connect with
}

func (a *Agent) sharesView(ctx context.Context) sharesView {
	l := a.layout()
	v := sharesView{Conf: a.smbConf(), Shares: []Share{}, Users: []SmbUser{}, Sessions: []smbConn{}, Root: l.Shares, Owner: l.ownerName()}
	v.Host, _ = os.Hostname()
	if _, err := os.Stat(a.smbConf()); err != nil {
		v.Error = "Samba isn't installed (no " + a.smbConf() + ")."
		return v
	}
	v.Samba = true
	users, err := a.smbUsers(ctx)
	if err != nil {
		v.Error = err.Error()
	} else {
		v.Users = users
	}
	st, managed := a.loadShares()
	v.Managed = managed
	if managed {
		v.Shares = st.Shares
	} else {
		b, _ := os.ReadFile(a.smbConf())
		_, secs := splitConf(string(b))
		var names []string
		for _, u := range users {
			names = append(names, u.Name)
		}
		for _, s := range secs {
			if specialSections[strings.ToLower(s.name)] {
				continue
			}
			sh := shareFromParams(s.name, parseParams(s.lines), names)
			for _, f := range a.alertsMoverShares() {
				if f != "" && f == a.layout().shareFolder(sh.Path) {
					sh.Mover = true
				}
			}
			v.Existing = append(v.Existing, sh)
		}
	}
	for _, s := range v.Shares {
		if _, err := os.Stat(a.sys(s.Path)); err != nil {
			v.Missing = append(v.Missing, s.Name)
		}
	}
	v.Sessions = a.smbSessions(ctx)
	now := time.Now()
	v.Checked = &now
	return v
}

// applyShares writes the include file, checks it with testparm, and reloads
// Samba. On any failure the previous files are put back.
func (a *Agent) applyShares(ctx context.Context, st sharesState) error {
	inc := a.smbInclude()
	old, hadOld := os.ReadFile(inc)
	if err := os.WriteFile(inc+".new", []byte(renderShares(st.Shares)), 0o644); err != nil {
		return err
	}
	if err := os.Rename(inc+".new", inc); err != nil {
		return err
	}
	restore := func() {
		if hadOld == nil {
			_ = os.WriteFile(inc, old, 0o644)
		} else {
			_ = os.Remove(inc)
		}
	}
	if out, err := a.cmd(ctx, 20*time.Second, "testparm", "-s", a.smbConf()); err != nil {
		restore()
		return fmt.Errorf("Samba rejected the settings, nothing was changed: %s", lastLine(out))
	}
	st.MoverInit = true
	if err := fans.WriteJSON(a.sharesPath(), st, 0o640); err != nil {
		restore()
		return errors.New("couldn't save the share settings")
	}
	if err := a.writeMoverShares(st); err != nil {
		return fmt.Errorf("shares saved, but the mover's list couldn't be written: %v", err)
	}
	_, _ = a.cmd(ctx, 20*time.Second, "smbcontrol", "smbd", "reload-config")
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for _, line := range slices.Backward(lines) {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "Load smb config") {
			return l
		}
	}
	return ""
}

// takeover moves the share sections out of smb.conf into the managed file.
func (a *Agent) takeover(ctx context.Context) error {
	if _, ok := a.loadShares(); ok {
		return errors.New("Shares are already managed here.")
	}
	b, err := os.ReadFile(a.smbConf())
	if err != nil {
		return err
	}
	users, _ := a.smbUsers(ctx)
	var names []string
	for _, u := range users {
		names = append(names, u.Name)
	}
	head, secs := splitConf(string(b))
	var keep []string
	keep = append(keep, head...)
	st := sharesState{Shares: []Share{}}
	for _, s := range secs {
		if specialSections[strings.ToLower(s.name)] {
			keep = append(keep, s.lines...)
			continue
		}
		st.Shares = append(st.Shares, shareFromParams(s.name, parseParams(s.lines), names))
	}
	cur := map[string]bool{}
	for _, f := range a.alertsMoverShares() {
		cur[f] = true
	}
	l := a.layout()
	for i := range st.Shares {
		f := l.shareFolder(st.Shares[i].Path)
		st.Shares[i].Mover = f != "" && cur[f]
	}
	st.MoverInit = true
	backup := fmt.Sprintf("%s.before-homestack-%s", a.smbConf(), time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backup, b, 0o644); err != nil {
		return err
	}
	text := strings.TrimRight(strings.Join(keep, "\n"), "\n") + "\n\n# Shares are managed by Homestack (Shares page):\ninclude = " + a.smbInclude() + "\n"
	if err := os.WriteFile(a.smbConf(), []byte(text), 0o644); err != nil {
		return err
	}
	if err := a.applyShares(ctx, st); err != nil {
		_ = os.WriteFile(a.smbConf(), b, 0o644)
		_ = os.Remove(a.sharesPath())
		return err
	}
	return nil
}

func (a *Agent) handleShares(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.sharesView(r.Context()))
}

func (a *Agent) postShares(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op       string `json:"op"` // takeover, save, delete, user-add, user-password, user-remove
		Share    *Share `json:"share"`
		Original string `json:"original"` // name being edited ("" = new)
		Create   bool   `json:"createFolder"`
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	ctx := r.Context()
	fail := func(err error) { writeErr(w, 400, err.Error()) }
	if req.Op == "takeover" {
		if err := a.takeover(ctx); err != nil {
			fail(err)
			return
		}
		writeJSON(w, 200, a.sharesView(ctx))
		return
	}
	st, managed := a.loadShares()
	users, err := a.smbUsers(ctx)
	if err != nil {
		fail(err)
		return
	}
	known := map[string]bool{}
	for _, u := range users {
		known[u.Name] = true
	}
	lay := a.layout()
	switch req.Op {
	case "save", "delete":
		if !managed {
			fail(errors.New("Take over the existing shares first."))
			return
		}
	}
	switch req.Op {
	case "save":
		if req.Share == nil {
			fail(errors.New("no share"))
			return
		}
		s := *req.Share
		if err := s.validate(lay, known); err != nil {
			fail(err)
			return
		}
		idx := -1
		for i, x := range st.Shares {
			if strings.EqualFold(x.Name, req.Original) && req.Original != "" {
				idx = i
			} else if strings.EqualFold(x.Name, s.Name) {
				fail(fmt.Errorf("There's already a share called %s.", x.Name))
				return
			}
		}
		if req.Original != "" && idx < 0 {
			fail(errors.New("That share no longer exists."))
			return
		}
		if _, err := os.Stat(a.sys(s.Path)); err != nil {
			if !req.Create {
				fail(fmt.Errorf("%s doesn't exist. Tick \"Create the folder\" to make it.", s.Path))
				return
			}
			if err := os.MkdirAll(a.sys(s.Path), 0o777); err != nil {
				fail(fmt.Errorf("Couldn't create %s: %v", s.Path, err))
				return
			}
			_ = os.Chmod(a.sys(s.Path), 0o777)
			uid, gid := lay.OwnerIDs()
			_ = os.Chown(a.sys(s.Path), uid, gid) // the shares' owner (Layout)
		}
		if idx >= 0 {
			st.Shares[idx] = s
		} else {
			st.Shares = append(st.Shares, s)
		}
	case "delete":
		out := st.Shares[:0]
		found := false
		for _, x := range st.Shares {
			if strings.EqualFold(x.Name, req.Original) {
				found = true
				continue
			}
			out = append(out, x)
		}
		if !found {
			fail(errors.New("That share no longer exists."))
			return
		}
		st.Shares = out
	case "user-add":
		if !reUserName.MatchString(req.User) || req.User == "root" {
			fail(errors.New("User names: lower-case letters, numbers, - and _, starting with a letter (up to 32)."))
			return
		}
		if known[req.User] {
			fail(errors.New("That user already exists."))
			return
		}
		if err := checkPassword(req.Password); err != nil {
			fail(err)
			return
		}
		if _, err := a.cmd(ctx, 5*time.Second, "id", "-u", req.User); err != nil {
			if out, err := a.cmd(ctx, 15*time.Second, "useradd", "-M", "-N", "-g", "users", "-s", "/usr/sbin/nologin", req.User); err != nil {
				fail(fmt.Errorf("useradd: %s", lastLine(out)))
				return
			}
			a.saveCreatedUsers(append(a.createdUsers(), req.User))
		}
		if out, err := a.cmdStdin(ctx, req.Password+"\n"+req.Password+"\n", "smbpasswd", "-a", "-s", req.User); err != nil {
			fail(fmt.Errorf("smbpasswd: %s", lastLine(out)))
			return
		}
	case "user-password":
		if !known[req.User] {
			fail(errors.New("Unknown user."))
			return
		}
		if err := checkPassword(req.Password); err != nil {
			fail(err)
			return
		}
		if out, err := a.cmdStdin(ctx, req.Password+"\n"+req.Password+"\n", "smbpasswd", "-s", req.User); err != nil {
			fail(fmt.Errorf("smbpasswd: %s", lastLine(out)))
			return
		}
		writeJSON(w, 200, a.sharesView(ctx))
		return
	case "user-remove":
		if !known[req.User] {
			fail(errors.New("Unknown user."))
			return
		}
		if out, err := a.cmd(ctx, 15*time.Second, "smbpasswd", "-x", req.User); err != nil {
			fail(fmt.Errorf("smbpasswd: %s", lastLine(out)))
			return
		}
		for i := range st.Shares {
			delete(st.Shares[i].Access, req.User)
		}
		var keep []string
		for _, u := range a.createdUsers() {
			if u == req.User {
				_, _ = a.cmd(ctx, 15*time.Second, "userdel", req.User)
				continue
			}
			keep = append(keep, u)
		}
		a.saveCreatedUsers(keep)
	default:
		fail(errors.New("unknown operation"))
		return
	}
	if managed {
		if err := a.applyShares(ctx, st); err != nil {
			fail(err)
			return
		}
	}
	writeJSON(w, 200, a.sharesView(ctx))
}

func checkPassword(p string) error {
	if len(p) < 8 || len(p) > 128 || strings.ContainsAny(p, "\n\r") {
		return errors.New("Passwords need at least 8 characters.")
	}
	return nil
}

// ---------- Who's connected ----------

type smbConn struct {
	User    string   `json:"user"`
	Machine string   `json:"machine"`
	Proto   string   `json:"protocol,omitempty"`
	Shares  []string `json:"shares"`
}

func (a *Agent) smbSessions(ctx context.Context) []smbConn {
	out, err := a.cmd(ctx, 10*time.Second, "smbstatus", "--json")
	conns := []smbConn{}
	if err != nil {
		return conns
	}
	var j struct {
		Sessions map[string]struct {
			Username       string `json:"username"`
			RemoteMachine  string `json:"remote_machine"`
			SessionDialect string `json:"session_dialect"`
		} `json:"sessions"`
		Tcons map[string]struct {
			Service   string `json:"service"`
			SessionID string `json:"session_id"`
		} `json:"tcons"`
	}
	start := strings.Index(out, "{")
	if start < 0 || json.Unmarshal([]byte(out[start:]), &j) != nil {
		return conns
	}
	byID := map[string]*smbConn{}
	var ids []string
	for id, s := range j.Sessions {
		byID[id] = &smbConn{User: s.Username, Machine: s.RemoteMachine, Proto: s.SessionDialect, Shares: []string{}}
		ids = append(ids, id)
	}
	for _, t := range j.Tcons {
		if c := byID[t.SessionID]; c != nil && t.Service != "IPC$" {
			c.Shares = append(c.Shares, t.Service)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := byID[id]
		sort.Strings(c.Shares)
		conns = append(conns, *c)
	}
	return conns
}
