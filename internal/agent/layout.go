package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"homestack/internal/fans"
)

// Layout is where the storage is mounted: the shares, the cache, the data
// and parity disks. It describes the mounts as they are (their lines in
// /etc/fstab, the pools' mountpoints): changing it moves nothing, it tells
// Homestack where to look. The defaults are Unraid's paths.
type Layout struct {
	Shares       string `json:"shares"`       // the cache and data disks as one (mergerfs): what shares and apps use
	Array        string `json:"array"`        // the data disks only (mergerfs): where the mover moves files to
	Cache        string `json:"cache"`        // the cache disk
	Appdata      string `json:"appdata"`      // apps' data, bound straight to the cache ("" = not a mount of its own)
	DiskPrefix   string `json:"diskPrefix"`   // data disk N is mounted at DiskPrefix+N, in a pool named after it
	ParityPrefix string `json:"parityPrefix"` // parity disk N is mounted at ParityPrefix+N
	Unassigned   string `json:"unassigned"`   // other disks, each in a folder of its own
	SnapRAID     string `json:"snapraid"`     // SnapRAID's config file
	Owner        string `json:"owner"`        // uid:gid that owns the shares' files
}

// DefaultLayout is Unraid's: /mnt/user, /mnt/cache, /mnt/disk1…, owned by
// nobody:users.
func DefaultLayout() Layout {
	return Layout{
		Shares: "/mnt/user", Array: "/mnt/user-array", Cache: "/mnt/cache", Appdata: "/mnt/user/appdata",
		DiskPrefix: "/mnt/disk", ParityPrefix: "/mnt/parity", Unassigned: "/mnt/disks",
		SnapRAID: "/etc/snapraid.conf", Owner: "99:100",
	}
}

func (l Layout) DiskPath(n int) string   { return l.DiskPrefix + strconv.Itoa(n) }
func (l Layout) DiskPool(n int) string   { return filepath.Base(l.DiskPrefix) + strconv.Itoa(n) }
func (l Layout) ParityPath(n int) string { return l.ParityPrefix + strconv.Itoa(n) }

// DiskNum is N for a data disk's mount point (DiskPrefix+N), 0 otherwise.
func (l Layout) DiskNum(p string) int { return numAfter(l.DiskPrefix, p) }

// ParityNum is N for a parity disk's mount point (ParityPrefix+N), 0 otherwise.
func (l Layout) ParityNum(p string) int { return numAfter(l.ParityPrefix, p) }

func numAfter(prefix, p string) int {
	rest, ok := strings.CutPrefix(strings.TrimRight(p, "/"), prefix)
	if !ok || rest == "" || len(rest) > 3 || rest[0] == '0' {
		return 0
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0
		}
	}
	n, _ := strconv.Atoi(rest)
	return n
}

// arrayMounts are the mounts above the pools that make the array, in the
// order they're mounted (stopping goes the other way).
func (l Layout) arrayMounts() []string {
	m := []string{l.Array, l.Shares}
	if l.Appdata != "" {
		m = append(m, l.Appdata)
	}
	return m
}

// OwnerIDs is who owns the shares' files.
func (l Layout) OwnerIDs() (uid, gid int) {
	u, g, ok := strings.Cut(l.Owner, ":")
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if !ok || err1 != nil || err2 != nil {
		return 99, 100
	}
	return uid, gid
}

// ownerName is the owner for people: "nobody:users (99:100)".
func (l Layout) ownerName() string {
	uid, gid := l.OwnerIDs()
	u, g := strconv.Itoa(uid), strconv.Itoa(gid)
	name, group := u, g
	if x, err := user.LookupId(u); err == nil {
		name = x.Username
	}
	if x, err := user.LookupGroupId(g); err == nil {
		group = x.Name
	}
	if name == u && group == g {
		return l.Owner
	}
	return name + ":" + group + " (" + l.Owner + ")"
}

// shareFolder is the share a path is the top folder of in the shares
// ("Media" for /mnt/user/Media), when the mover can handle it: one folder
// deep, nothing a shell would trip on. "" otherwise.
func (l Layout) shareFolder(p string) string {
	rest, ok := strings.CutPrefix(filepath.Clean(p), l.Shares+"/")
	if !ok || rest == "" || strings.Contains(rest, "/") || strings.ContainsAny(rest, " \"'$`\\") {
		return ""
	}
	return rest
}

// inStorage says whether p is inside the shares, the cache, a data disk or
// an unassigned disk (where a share's folder may be).
func (l Layout) inStorage(p string) bool {
	p = filepath.Clean(p)
	for _, r := range []string{l.Shares, l.Cache, l.Unassigned} {
		if strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	if rest, ok := strings.CutPrefix(p, l.DiskPrefix); ok {
		if i := strings.IndexByte(rest, '/'); i > 0 && i < len(rest)-1 && numAfter("", rest[:i]) > 0 {
			return true
		}
	}
	return false
}

// where names the disk a path is on, for people: "the cache", "disk3", or
// "" when it's elsewhere.
func (l Layout) where(p string) string {
	p = filepath.Clean(p)
	if p == l.Cache || strings.HasPrefix(p, l.Cache+"/") {
		return "the cache"
	}
	if rest, ok := strings.CutPrefix(p, l.DiskPrefix); ok {
		num, _, _ := strings.Cut(rest, "/")
		if n := numAfter("", num); n > 0 {
			return filepath.Base(l.DiskPath(n))
		}
	}
	return ""
}

// ownsMount says whether a mount point is the storage's (the shares, the
// cache, the disks, or anything else in the folders they're in, like
// /mnt), which a restore brings back to /etc/fstab.
func (l Layout) ownsMount(t string) bool {
	t = filepath.Clean(t)
	for _, p := range []string{l.Shares, l.Array, l.Cache, l.Appdata} {
		if p != "" && t == p {
			return true
		}
	}
	if l.DiskNum(t) > 0 || l.ParityNum(t) > 0 {
		return true
	}
	for _, p := range []string{l.Shares, l.Array, l.Cache, l.DiskPrefix, l.ParityPrefix, l.Unassigned + "/x"} {
		if d := filepath.Dir(p); d != "/" && under(t, d) {
			return true
		}
	}
	return false
}

// ---------- Checks ----------

var (
	reLayoutPath   = regexp.MustCompile(`^(/[A-Za-z0-9_.-]+)+$`)
	rePrefixBase   = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_.-]*[A-Za-z_.-])?$`)
	reOwner        = regexp.MustCompile(`^(\d{1,5}):(\d{1,5})$`)
	systemDirs     = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/tmp", "/usr", "/var"}
	reservedPrefix = []string{"mirror", "raidz", "draid", "spare", "log"}
)

func under(p, dir string) bool { return strings.HasPrefix(p, dir+"/") }

// validate checks a layout on its own: full paths, nothing in the system's
// folders, nothing on top of anything else.
func (l *Layout) validate() error {
	type field struct {
		name string
		p    *string
	}
	mounts := []field{
		{"The shares", &l.Shares}, {"The array", &l.Array}, {"The cache", &l.Cache},
		{"Unassigned disks", &l.Unassigned}, {"Data disks", &l.DiskPrefix}, {"Parity disks", &l.ParityPrefix},
	}
	if l.Appdata = strings.TrimSpace(l.Appdata); l.Appdata != "" {
		mounts = append(mounts, field{"Apps' data", &l.Appdata})
	}
	for _, f := range mounts {
		*f.p = strings.TrimSpace(*f.p)
		p := *f.p
		if !reLayoutPath.MatchString(p) || filepath.Clean(p) != p {
			return fmt.Errorf("%s: use a full path like /mnt/user (letters, numbers, . _ and - only).", f.name)
		}
		for _, d := range systemDirs {
			if p == d || under(p, d) {
				return fmt.Errorf("%s: %s is one of the system's own folders.", f.name, p)
			}
		}
	}
	for _, f := range []field{{"Data disks", &l.DiskPrefix}, {"Parity disks", &l.ParityPrefix}} {
		base := filepath.Base(*f.p)
		if !rePrefixBase.MatchString(base) {
			return fmt.Errorf("%s: the name before the number (%s) has to start with a letter and not end in a digit, like /mnt/disk.", f.name, base)
		}
	}
	low := strings.ToLower(filepath.Base(l.DiskPrefix))
	for _, r := range reservedPrefix {
		if strings.HasPrefix(low, r) {
			return fmt.Errorf("Data disks: pools can't be named %s1, %s2… (ZFS keeps names starting with %q).", low, low, r)
		}
	}
	if low == "c" {
		return errors.New("Data disks: pools can't be named c1, c2… (ZFS keeps those names).")
	}
	if strings.HasPrefix(l.DiskPrefix, l.ParityPrefix) || strings.HasPrefix(l.ParityPrefix, l.DiskPrefix) {
		return errors.New("Data disks and parity disks need names that don't start with each other, like /mnt/disk and /mnt/parity.")
	}
	single := []field{{"The shares", &l.Shares}, {"The array", &l.Array}, {"The cache", &l.Cache}, {"Unassigned disks", &l.Unassigned}}
	for i, a := range single {
		if n := l.DiskNum(*a.p); n > 0 {
			return fmt.Errorf("%s: %s is where data disk %d goes.", a.name, *a.p, n)
		}
		if n := l.ParityNum(*a.p); n > 0 {
			return fmt.Errorf("%s: %s is where parity disk %d goes.", a.name, *a.p, n)
		}
		for _, b := range single[i+1:] {
			if *a.p == *b.p || under(*a.p, *b.p) || under(*b.p, *a.p) {
				return fmt.Errorf("%s and %s can't be in the same place (%s, %s).", a.name, strings.ToLower(b.name), *a.p, *b.p)
			}
		}
		for _, pre := range []field{{"data disks", &l.DiskPrefix}, {"parity disks", &l.ParityPrefix}} {
			if under(*pre.p+"1", *a.p) {
				return fmt.Errorf("The %s would be inside %s (%s).", pre.name, strings.ToLower(a.name), *a.p)
			}
		}
	}
	if l.Appdata != "" && !under(l.Appdata, l.Shares) {
		return fmt.Errorf("Apps' data: %s has to be a folder in the shares (%s), like %s/appdata.", l.Appdata, l.Shares, l.Shares)
	}
	l.SnapRAID = strings.TrimSpace(l.SnapRAID)
	if !reLayoutPath.MatchString(l.SnapRAID) || filepath.Clean(l.SnapRAID) != l.SnapRAID {
		return errors.New("SnapRAID's config: use a full path to the file, like /etc/snapraid.conf.")
	}
	l.Owner = strings.TrimSpace(l.Owner)
	m := reOwner.FindStringSubmatch(l.Owner)
	if m == nil {
		return errors.New("Owner: use the user and group ID, like 99:100.")
	}
	for _, id := range m[1:] {
		if n, _ := strconv.Atoi(id); n > 65534 {
			return errors.New("Owner: IDs go up to 65534.")
		}
	}
	return nil
}

// ---------- Saved ----------

func (a *Agent) layoutPath() string { return filepath.Join(a.opt.StateDir, "layout.json") }

// layoutFile is the layout for the shell jobs (the mover, the alerts).
func (a *Agent) layoutFile() string {
	return filepath.Join(filepath.Dir(a.alertsConf()), "homestack-layout")
}

type layoutCache struct {
	sync.Mutex
	v *Layout
}

// layout is the saved layout, or the default.
func (a *Agent) layout() Layout {
	a.lay.Lock()
	defer a.lay.Unlock()
	if a.lay.v == nil {
		l := DefaultLayout()
		if b, err := os.ReadFile(a.layoutPath()); err == nil {
			var saved Layout
			if json.Unmarshal(b, &saved) == nil && saved.validate() == nil {
				l = saved
			}
		}
		a.lay.v = &l
	}
	return *a.lay.v
}

// setLayout saves a checked layout, for the agent and the shell jobs.
func (a *Agent) setLayout(l Layout) error {
	if err := fans.WriteJSON(a.layoutPath(), l, 0o644); err != nil {
		return err
	}
	return a.useLayout(l)
}

// useLayout switches to a layout that's been saved.
func (a *Agent) useLayout(l Layout) error {
	a.lay.Lock()
	a.lay.v = &l
	a.lay.Unlock()
	return a.writeLayoutFile(l)
}

// shellQuote single-quotes s for a shell, quotes inside it included.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (a *Agent) writeLayoutFile(l Layout) error {
	q := shellQuote
	b := "# Written by Homestack (Settings → Storage → Layout). The mover and the alert jobs read it.\n" +
		"STORAGE_SHARES=" + q(l.Shares) + "\n" +
		"STORAGE_ARRAY=" + q(l.Array) + "\n" +
		"STORAGE_CACHE=" + q(l.Cache) + "\n" +
		"STORAGE_APPDATA=" + q(l.Appdata) + "\n" +
		"STORAGE_DISK_PREFIX=" + q(l.DiskPrefix) + "\n" +
		"STORAGE_PARITY_PREFIX=" + q(l.ParityPrefix) + "\n" +
		"STORAGE_UNASSIGNED=" + q(l.Unassigned) + "\n" +
		"STORAGE_SNAPRAID=" + q(l.SnapRAID) + "\n" +
		"STORAGE_OWNER=" + q(l.Owner) + "\n"
	if a.opt.DataDir != "" { // Homestack's settings, for notify-discord while its container is down
		b += "HOMESTACK_DATA=" + q(a.opt.DataDir) + "\n"
	}
	path := a.layoutFile()
	if old, err := os.ReadFile(path); err == nil && string(old) == b {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(b), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- Settings → Storage ----------

// LayoutView is the Layout card.
type LayoutView struct {
	Layout    Layout            `json:"layout"`
	Default   Layout            `json:"default"`
	OwnerName string            `json:"ownerName"`
	Mounted   map[string]string `json:"mounted"` // path → what's mounted there ("" = nothing)
	Disks     []string          `json:"disks"`   // the data disks in snapraid.conf
	Parity    []string          `json:"parity"`  // and the parity disks
	Busy      string            `json:"busy,omitempty"`
}

func (a *Agent) layoutView(ctx context.Context) LayoutView {
	l := a.layout()
	v := LayoutView{Layout: l, Default: DefaultLayout(), OwnerName: l.ownerName(), Mounted: map[string]string{}, Disks: []string{}, Parity: []string{}, Busy: a.layoutBusy()}
	for _, p := range []string{l.Shares, l.Array, l.Cache, l.Appdata} {
		if p != "" {
			v.Mounted[p] = a.mountType(ctx, p)
		}
	}
	if conf, err := readSnapConf(a.snapraidConf()); err == nil {
		for _, d := range conf.data {
			v.Disks = append(v.Disks, strings.TrimRight(d.Path, "/"))
		}
		for _, p := range conf.parity {
			v.Parity = append(v.Parity, parentMount(p.Path))
		}
	}
	return v
}

// mountType is the filesystem mounted at p ("" when nothing is).
func (a *Agent) mountType(ctx context.Context, p string) string {
	o, err := a.cmd(ctx, 5*time.Second, "findmnt", "-no", "FSTYPE", "--mountpoint", p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(o)
}

// layoutBusy says why the layout can't change now: a disk job keeps paths
// from when it started, and a storage task is working in them.
func (a *Agent) layoutBusy() string {
	if a.opNow().active() {
		return "A disk job is in progress. Change the layout once it's finished."
	}
	if h := a.jobs.lockHolder(); h != "" {
		return h + " is running. Change the layout once it's finished."
	}
	return ""
}

// checkLayout checks a new layout against the server as it is: whatever
// moved has to be mounted there now, so Homestack is never pointed at an
// empty folder on the boot drive (the mover would fill it).
func (a *Agent) checkLayout(ctx context.Context, old, l Layout) error {
	if err := l.validate(); err != nil {
		return err
	}
	if b := a.layoutBusy(); b != "" {
		return errors.New(b)
	}
	views := []struct{ name, old, new string }{{"The shares", old.Shares, l.Shares}, {"The array", old.Array, l.Array}}
	for _, v := range views {
		if v.new == v.old {
			continue
		}
		switch t := a.mountType(ctx, v.new); {
		case t == "":
			return fmt.Errorf("%s: nothing is mounted at %s. Mount it first (it's a mergerfs view, in /etc/fstab).", v.name, v.new)
		case t != "fuse.mergerfs":
			return fmt.Errorf("%s: %s is %s, not a mergerfs view of the disks.", v.name, v.new, t)
		}
	}
	if l.Cache != old.Cache && a.mountType(ctx, l.Cache) == "" {
		return fmt.Errorf("The cache: nothing is mounted at %s.", l.Cache)
	}
	if l.Appdata != "" && l.Appdata != old.Appdata && a.mountType(ctx, l.Appdata) == "" {
		return fmt.Errorf("Apps' data: nothing is mounted at %s. Leave it empty if apps' data isn't a mount of its own.", l.Appdata)
	}
	path := a.snapraidConf()
	if l.SnapRAID != old.SnapRAID {
		path = l.SnapRAID
	}
	conf, err := readSnapConf(path)
	if l.SnapRAID != old.SnapRAID && err != nil {
		return fmt.Errorf("SnapRAID's config: couldn't read %s.", l.SnapRAID)
	}
	if err != nil {
		return nil // no SnapRAID yet: nothing to match the disks against
	}
	if l.DiskPrefix != old.DiskPrefix || l.SnapRAID != old.SnapRAID {
		for _, d := range conf.data {
			if l.DiskNum(d.Path) == 0 {
				return fmt.Errorf("Data disks: snapraid.conf has %s at %s, which isn't %sN.", d.Name, strings.TrimRight(d.Path, "/"), l.DiskPrefix)
			}
		}
	}
	if l.ParityPrefix != old.ParityPrefix || l.SnapRAID != old.SnapRAID {
		for _, p := range conf.parity {
			if l.ParityNum(parentMount(p.Path)) == 0 {
				return fmt.Errorf("Parity disks: snapraid.conf has %s on %s, which isn't %sN.", p.Name, parentMount(p.Path), l.ParityPrefix)
			}
		}
	}
	return nil
}

func (a *Agent) getLayout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.layoutView(r.Context()))
}

func (a *Agent) putLayout(w http.ResponseWriter, r *http.Request) {
	var l Layout
	if err := readBody(r, &l); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if err := l.validate(); err != nil { // and tidies it
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.checkLayout(r.Context(), a.layout(), l); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.setLayout(l); err != nil {
		writeErr(w, 500, "Couldn't save the layout: "+err.Error())
		return
	}
	writeJSON(w, 200, a.layoutView(r.Context()))
}
