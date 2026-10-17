package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Changing the host's network safely. Every change, whether from the Edit
// form, the VLAN wizard, the file editor or a backup, goes through one
// pipeline:
//
//  1. plan: work out the new files and show exactly what changes;
//  2. refuse what would obviously cut the server off (taking down the link
//     you're connected through, removing the address you're using, a
//     .network file that would claim every interface, …);
//  3. snapshot /etc/systemd/network, write the files, `networkctl reload`;
//  4. test: every interface that could reach its gateway before must still
//     reach it. If one can't, undo straight away;
//  5. wait for a confirmation from the browser, which proves the web page
//     still works. Without one inside the window, undo.
//
// The agent owns the timer, so the undo happens even when Homestack can't
// be reached any more, and an unconfirmed change is also undone if the
// agent stops or restarts.

const netConfirmWindow = 90 * time.Second

const netBackupsKept = 20

type netManager struct {
	a *Agent

	mu      sync.Mutex // one change at a time
	pending *netPending
	timer   *time.Timer

	// How long to let interfaces settle after a change, and how long to keep
	// re-testing gateways before giving up (shortened in tests).
	settle, checkFor time.Duration

	stmu sync.Mutex // the fields below are read by the status view
	busy string
	last *NetEvent
}

func newNetManager(a *Agent) *netManager {
	n := &netManager{a: a, settle: 2 * time.Second, checkFor: 15 * time.Second}
	// What happened last survives a restart, so the page can still say why
	// a change was undone.
	if b, err := os.ReadFile(n.lastPath()); err == nil {
		var e NetEvent
		if json.Unmarshal(b, &e) == nil {
			n.last = &e
		}
	}
	return n
}

// GatewayCheck is one "can this interface still reach its gateway" test.
type GatewayCheck struct {
	Link    string `json:"link"`
	From    string `json:"from"`
	Gateway string `json:"gateway"`
	Before  bool   `json:"before"`
	After   bool   `json:"after"`
}

type netPending struct {
	Token    string          `json:"token"`
	Summary  string          `json:"summary"`
	Deadline time.Time       `json:"deadline"`
	Backup   string          `json:"backup"`
	LinkUndo map[string]bool `json:"linkUndo,omitempty"` // bring up (true) or down (false) on undo
	Warnings []string        `json:"warnings,omitempty"`
	Checks   []GatewayCheck  `json:"checks,omitempty"`
}

// PendingView is a change waiting to be kept or undone.
type PendingView struct {
	Token    string         `json:"token"`
	Summary  string         `json:"summary"`
	Seconds  int            `json:"seconds"`
	Warnings []string       `json:"warnings"`
	Checks   []GatewayCheck `json:"checks"`
}

// NetEvent is the last thing that happened, so a page that lost its
// connection can say what became of the change.
type NetEvent struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // ok | warning
	Text  string    `json:"text"`
}

// NetBackup is one saved copy of /etc/systemd/network.
type NetBackup struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Summary string    `json:"summary"`
	Files   int       `json:"files"`
}

// NetView is everything the Host tab shows.
type NetView struct {
	Managed bool         `json:"managed"`
	Note    string       `json:"note,omitempty"`
	Dir     string       `json:"dir"`
	Links   []NetLink    `json:"links"`
	Routes  []NetRoute   `json:"routes"`
	Rules   []NetRule    `json:"rules"`
	DNS     []string     `json:"dns"`
	Files   []NetFile    `json:"files"`
	Pending *PendingView `json:"pending,omitempty"`
	Busy    string       `json:"busy,omitempty"`
	Last    *NetEvent    `json:"last,omitempty"`
	Backups []NetBackup  `json:"backups"`
	Window  int          `json:"window"` // seconds to confirm a change
}

// NetOp is a requested change.
type NetOp struct {
	Op          string             `json:"op"` // link-config | link-state | vlan-add | vlan-remove | files | restore
	Link        string             `json:"link,omitempty"`
	Config      *LinkConfigEdit    `json:"config,omitempty"`
	Up          *bool              `json:"up,omitempty"`
	Parent      string             `json:"parent,omitempty"`
	VLAN        int                `json:"vlan,omitempty"`
	Bridge      string             `json:"bridge,omitempty"`
	Address     string             `json:"address,omitempty"`
	Files       map[string]*string `json:"files,omitempty"` // name -> content; null deletes
	Backup      string             `json:"backup,omitempty"`
	Via         string             `json:"via,omitempty"`         // the address the browser reached Homestack on
	ConfirmText string             `json:"confirmText,omitempty"` // typed by the person, when asked
}

// FileChange is one file in the review screen.
type FileChange struct {
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// NetPlan is what a change would do, for the review screen.
type NetPlan struct {
	Summary     string          `json:"summary"`
	Files       []FileChange    `json:"files"`
	LinkOps     map[string]bool `json:"linkOps,omitempty"`
	Remove      []string        `json:"remove,omitempty"` // virtual interfaces that get deleted
	Notes       []string        `json:"notes"`
	Critical    string          `json:"critical,omitempty"`    // your connection depends on this interface
	ConfirmWord string          `json:"confirmWord,omitempty"` // type this to apply
	Seconds     int             `json:"seconds"`
}

type netChange struct {
	summary  string
	files    map[string]*string
	linkOps  map[string]bool
	notes    []string
	critical string
}

// netError carries an HTTP status with a message for the person.
type netError struct {
	status int
	msg    string
}

func (e *netError) Error() string { return e.msg }

func netErr(status int, format string, args ...any) error {
	return &netError{status, fmt.Sprintf(format, args...)}
}

func (n *netManager) setBusy(s string) {
	n.stmu.Lock()
	n.busy = s
	n.stmu.Unlock()
}

func (n *netManager) event(level, text string) {
	log.Printf("network: %s", text)
	n.stmu.Lock()
	n.last = &NetEvent{Time: time.Now(), Level: level, Text: text}
	b, _ := json.Marshal(n.last)
	n.stmu.Unlock()
	if err := writeFileAtomic(n.lastPath(), b, 0o600); err != nil {
		log.Printf("network: saving the last event: %v", err)
	}
}

func (n *netManager) lastPath() string {
	return filepath.Join(n.a.opt.StateDir, "network-last.json")
}

// ---------- Planning ----------

func (n *netManager) plan(ctx context.Context, op NetOp) (netChange, netState, error) {
	st := n.readNetState(ctx)
	if !st.managed {
		return netChange{}, st, netErr(409, "%s", st.note)
	}
	yours, crit := st.critical(op.Via)
	ch := netChange{files: map[string]*string{}, linkOps: map[string]bool{}}
	set := func(name, text string) { t := text; ch.files[name] = &t }

	switch op.Op {
	case "link-config":
		l := st.link(op.Link)
		if l == nil {
			return ch, st, netErr(404, "There's no interface called %s.", op.Link)
		}
		text, ok := st.files[l.NetworkFile]
		if l.NetworkFile == "" || !ok {
			return ch, st, netErr(409, "%s isn't set up by a file in %s, so there's nothing for the form to edit. Create one in the file editor.", l.Name, n.dir())
		}
		if op.Config == nil {
			return ch, st, netErr(400, "No settings were sent.")
		}
		cur := readLinkConfig(l.NetworkFile, text)
		if l.Name == yours {
			via, _ := netip.ParseAddr(op.Via)
			still := false
			for _, a := range op.Config.Addresses {
				if p, err := netip.ParsePrefix(a.Now); err == nil && p.Addr() == via.Unmap() {
					still = true
				}
			}
			if !still && slices.ContainsFunc(cur.Addresses, func(s string) bool { p, err := netip.ParsePrefix(s); return err == nil && p.Addr() == via.Unmap() }) {
				return ch, st, netErr(409, "You're connected to Homestack through %s, so it can't be removed or changed from here: the page would lose its connection. To move it safely, add the new address first, open Homestack on the new address, then remove the old one.", op.Via)
			}
			if op.Config.DHCP != "no" && op.Config.DHCP != "" && cur.DHCP == "no" {
				return ch, st, netErr(409, "You're connected through %s. Switching it to DHCP could give it a different address and cut this page off. Do it from another address or the console.", l.Name)
			}
		}
		out, notes, err := applyLinkConfig(text, *op.Config)
		if err != nil {
			return ch, st, netErr(400, "%s", capFirst(err.Error()))
		}
		if out == text {
			return ch, st, netErr(409, "%s", capFirst(errNoChange.Error()))
		}
		set(l.NetworkFile, out)
		ch.notes = append(ch.notes, notes...)
		var others []string
		for _, o := range st.links {
			if o.Name != l.Name && o.NetworkFile == l.NetworkFile {
				others = append(others, o.Name)
			}
		}
		if len(others) > 0 {
			ch.notes = append(ch.notes, fmt.Sprintf("%s also sets up %s, so they change too.", l.NetworkFile, strings.Join(others, ", ")))
		}
		ch.summary = "Change the settings of " + l.Name

	case "link-state":
		l := st.link(op.Link)
		if l == nil {
			return ch, st, netErr(404, "There's no interface called %s.", op.Link)
		}
		if op.Up == nil {
			return ch, st, netErr(400, "Say whether to bring it up or take it down.")
		}
		if l.Type == "loopback" || l.Type == "veth" || l.Group != "" {
			return ch, st, netErr(409, "%s is managed automatically (by Docker, libvirt or the system), not by hand.", l.Name)
		}
		if !*op.Up && crit[l.Name] {
			if l.Name == yours {
				return ch, st, netErr(409, "You're connected to Homestack through %s, so taking it down would cut this page off. Use another link or the console.", l.Name)
			}
			return ch, st, netErr(409, "Your connection to Homestack runs through %s, so taking it down would cut this page off. Use another link or the console.", l.Name)
		}
		if l.Up == *op.Up {
			return ch, st, netErr(409, "%s is already %s.", l.Name, map[bool]string{true: "up", false: "down"}[l.Up])
		}
		ch.linkOps[l.Name] = *op.Up
		if *op.Up {
			ch.summary = "Bring " + l.Name + " up"
		} else {
			ch.summary = "Take " + l.Name + " down"
			ch.notes = append(ch.notes, fmt.Sprintf("Anything using %s stops reaching the network. It stays down until you bring it up again or the server restarts.", l.Name))
		}

	case "vlan-add":
		if err := n.planVLANAdd(&st, op, crit, &ch); err != nil {
			return ch, st, err
		}

	case "vlan-remove":
		if err := n.planVLANRemove(&st, op, crit, &ch); err != nil {
			return ch, st, err
		}

	case "files":
		if len(op.Files) == 0 {
			return ch, st, netErr(400, "No files were sent.")
		}
		var made, edited, deleted []string
		for _, name := range slices.Sorted(maps.Keys(op.Files)) {
			text := op.Files[name]
			old, exists := st.files[name]
			if !reNetFile.MatchString(name) {
				return ch, st, netErr(400, "%q isn't a valid file name: use letters, digits and . _ @ -, ending in .network, .netdev or .link.", name)
			}
			switch {
			case text == nil && !exists:
				return ch, st, netErr(404, "There's no %s to delete.", name)
			case text == nil:
				ch.files[name] = nil
				deleted = append(deleted, name)
			default:
				t := strings.ReplaceAll(*text, "\r\n", "\n")
				if t != "" && !strings.HasSuffix(t, "\n") {
					t += "\n"
				}
				if err := validateNetFile(name, t); err != nil {
					return ch, st, netErr(400, "%s", capFirst(err.Error()))
				}
				if exists && t == old {
					continue
				}
				set(name, t)
				if exists {
					edited = append(edited, name)
				} else {
					made = append(made, name)
				}
			}
		}
		if len(ch.files) == 0 {
			return ch, st, netErr(409, "%s", capFirst(errNoChange.Error()))
		}
		var parts []string
		for _, p := range []struct {
			verb  string
			names []string
		}{{"Edit", edited}, {"Create", made}, {"Delete", deleted}} {
			if len(p.names) > 0 {
				parts = append(parts, p.verb+" "+strings.Join(p.names, ", "))
			}
		}
		ch.summary = strings.Join(parts, "; ")

	case "restore":
		files, meta, err := n.loadBackup(op.Backup)
		if err != nil {
			return ch, st, err
		}
		for name, old := range st.files {
			if _, ok := files[name]; !ok {
				ch.files[name] = nil
			} else if files[name] == old {
				delete(files, name)
			}
		}
		for name, text := range files {
			if _, deleting := ch.files[name]; !deleting {
				set(name, text)
			}
		}
		if len(ch.files) == 0 {
			return ch, st, netErr(409, "The network files already match that backup.")
		}
		ch.summary = "Restore the network files from " + meta.Time.Local().Format("2 Jan 15:04")
		if meta.Summary != "" {
			ch.summary += " (" + strings.TrimPrefix(meta.Summary, "Before: ") + ")"
		}

	default:
		return ch, st, netErr(400, "Unknown change %q.", op.Op)
	}

	// Checks on the configuration as a whole once the change is made.
	after := maps.Clone(st.files)
	for name, text := range ch.files {
		if text == nil {
			delete(after, name)
		} else {
			after[name] = *text
		}
	}
	nets := 0
	devs := map[string]string{}
	for name, text := range after {
		if strings.HasSuffix(name, ".network") {
			nets++
		}
		if strings.HasSuffix(name, ".netdev") {
			dev := netdevName(text)
			if other, dup := devs[dev]; dup {
				return ch, st, netErr(400, "%s and %s both create %s.", other, name, dev)
			}
			devs[dev] = name
		}
	}
	if nets == 0 && len(st.files) > 0 {
		return ch, st, netErr(409, "That would leave no .network files at all, so the server would have no network settings. Keep at least the ones for the links you use.")
	}

	// Does this touch what your connection depends on?
	if ch.critical == "" {
		for l := range ch.linkOps {
			if crit[l] {
				ch.critical = l
			}
		}
	}
	if ch.critical == "" {
		for name, text := range ch.files {
			for _, t := range []string{st.files[name], deref(text)} {
				for _, l := range fileTouches(name, t, st) {
					if crit[l] && ch.critical == "" {
						ch.critical = l
					}
				}
			}
		}
	}
	if ch.critical != "" {
		who := "Your connection to Homestack depends on " + ch.critical
		if ch.critical == yours {
			who = "You're connected to Homestack through " + ch.critical
		}
		ch.notes = append([]string{who + ". If this change breaks it, this page stops responding and the change is undone after " + strconv.Itoa(int(netConfirmWindow.Seconds())) + " seconds."}, ch.notes...)
	}
	return ch, st, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// fileTouches lists the interfaces a file applies to. When it can't be told
// (a [Match] on driver, path, type, a negation…), it says every interface,
// to stay on the safe side.
func fileTouches(name, text string, st netState) []string {
	if text == "" {
		return nil
	}
	all := func() []string {
		var out []string
		for _, l := range st.links {
			out = append(out, l.Name)
		}
		return out
	}
	if strings.HasSuffix(name, ".netdev") {
		return []string{netdevName(text)}
	}
	d := parseNC(text)
	var names, macs []string
	for _, s := range d.sectionsNamed("Match") {
		for i := s.start + 1; i < s.end; i++ {
			k, v, ok := ncKV(d.lines[i])
			if !ok {
				continue
			}
			switch k {
			case "Name", "OriginalName":
				names = append(names, strings.Fields(v)...)
			case "MACAddress", "PermanentMACAddress":
				for _, m := range strings.Fields(v) {
					macs = append(macs, strings.ToLower(m))
				}
			default:
				return all()
			}
		}
	}
	var out []string
	for _, l := range st.links {
		hit := false
		for _, p := range names {
			if strings.HasPrefix(p, "!") {
				return all()
			}
			if ok, _ := path.Match(p, l.Name); ok {
				hit = true
			}
		}
		if slices.Contains(macs, strings.ToLower(l.MAC)) || (l.permMAC != "" && slices.Contains(macs, strings.ToLower(l.permMAC))) {
			hit = true
		}
		if hit {
			out = append(out, l.Name)
		}
	}
	return out
}

func (n *netManager) planVLANAdd(st *netState, op NetOp, crit map[string]bool, ch *netChange) error {
	p := st.link(op.Parent)
	if p == nil {
		return netErr(404, "There's no interface called %s.", op.Parent)
	}
	if p.Type != "port" && p.Type != "bond" {
		return netErr(400, "A VLAN goes on a network port or a bond (the link your switch sends tagged VLANs down), not on a %s.", p.Type)
	}
	text, ok := st.files[p.NetworkFile]
	if p.NetworkFile == "" || !ok {
		return netErr(409, "%s isn't set up by a file in %s, so the VLAN can't be added to it here.", p.Name, n.dir())
	}
	if op.VLAN < 1 || op.VLAN > 4094 {
		return netErr(400, "A VLAN ID is a number from 1 to 4094.")
	}
	for _, l := range st.links {
		if l.Type == "vlan" && l.Parent == p.Name && l.VLAN == op.VLAN {
			return netErr(409, "VLAN %d is already set up on %s (%s).", op.VLAN, p.Name, l.Name)
		}
	}
	vlan := p.Name + "." + strconv.Itoa(op.VLAN)
	if len(vlan) > 15 {
		vlan = "vlan" + strconv.Itoa(op.VLAN)
	}
	bridge := strings.TrimSpace(op.Bridge)
	if bridge == "" {
		bridge = defaultVLANBridge(*p, op.VLAN)
	}
	if !reIfName.MatchString(bridge) {
		return netErr(400, "%q isn't a valid interface name: up to 15 letters, digits and . _ -.", bridge)
	}
	taken := map[string]bool{}
	for _, l := range st.links {
		taken[l.Name] = true
	}
	for name, t := range st.files {
		if strings.HasSuffix(name, ".netdev") {
			taken[netdevName(t)] = true
		}
	}
	for _, name := range []string{vlan, bridge} {
		if taken[name] {
			return netErr(409, "There's already an interface called %s.", name)
		}
	}
	var addr string
	if a := strings.TrimSpace(op.Address); a != "" {
		pfx, err := validCIDR(a)
		if err != nil {
			return netErr(400, "%s", capFirst(err.Error()))
		}
		for _, l := range st.links {
			for _, la := range l.Addrs {
				if q, err := netip.ParsePrefix(la.Addr); err == nil && la.Scope == "global" && q.Overlaps(pfx) {
					return netErr(409, "%s overlaps %s on %s. A VLAN needs its own network.", pfx.Masked(), q.Masked(), l.Name)
				}
			}
		}
		addr = pfx.String()
	}
	files := map[string]string{
		"30-" + vlan + ".netdev":    fmt.Sprintf("# VLAN %d on %s (made by Homestack).\n[NetDev]\nName=%s\nKind=vlan\n\n[VLAN]\nId=%d\n", op.VLAN, p.Name, vlan, op.VLAN),
		"30-" + vlan + ".network":   fmt.Sprintf("# VLAN %d's traffic goes into the bridge %s.\n[Match]\nName=%s\n\n[Network]\nBridge=%s\n", op.VLAN, bridge, vlan, bridge),
		"30-" + bridge + ".netdev":  fmt.Sprintf("# Bridge for VLAN %d (made by Homestack). VMs join it to sit on that VLAN.\n[NetDev]\nName=%s\nKind=bridge\n\n[Bridge]\nSTP=no\nForwardDelaySec=0\n", op.VLAN, bridge),
		"30-" + bridge + ".network": fmt.Sprintf("[Match]\nName=%s\n\n[Network]\nLinkLocalAddressing=no\nIPv6AcceptRA=no\nConfigureWithoutCarrier=yes\n", bridge),
	}
	if addr != "" {
		files["30-"+bridge+".network"] = fmt.Sprintf("[Match]\nName=%s\n\n[Network]\nAddress=%s\nIPv6AcceptRA=no\nConfigureWithoutCarrier=yes\n", bridge, addr)
	}
	for name, t := range files {
		if _, exists := st.files[name]; exists {
			return netErr(409, "%s already exists.", name)
		}
		if err := validateNetFile(name, t); err != nil {
			return netErr(500, "%s", err.Error())
		}
		t := t
		ch.files[name] = &t
	}
	d := parseNC(text)
	d.add(d.ensure("Network"), "VLAN", vlan)
	out := d.String()
	ch.files[p.NetworkFile] = &out
	ch.summary = fmt.Sprintf("Add VLAN %d on %s, with the bridge %s for VMs", op.VLAN, p.Name, bridge)
	ch.notes = append(ch.notes, fmt.Sprintf("The switch port %s is plugged into must send VLAN %d to this server tagged, or VMs on %s won't reach anything.", p.Name, op.VLAN, bridge))
	if addr == "" {
		ch.notes = append(ch.notes, fmt.Sprintf("The server itself gets no address on VLAN %d: only VMs use it, and the server isn't reachable there.", op.VLAN))
	}
	if crit[p.Name] {
		ch.critical = p.Name
	}
	return nil
}

// defaultVLANBridge names a VLAN's bridge after the bridge its parent is in,
// the way Unraid does (br0 → br0.40), or after the VLAN.
func defaultVLANBridge(p NetLink, id int) string {
	if p.Master != "" {
		if b := p.Master + "." + strconv.Itoa(id); len(b) <= 15 {
			return b
		}
	}
	return "brvlan" + strconv.Itoa(id)
}

func (n *netManager) planVLANRemove(st *netState, op NetOp, crit map[string]bool, ch *netChange) error {
	b := st.link(op.Bridge)
	if b == nil {
		return netErr(404, "There's no interface called %s.", op.Bridge)
	}
	if b.Type != "bridge" {
		return netErr(400, "%s isn't a bridge.", b.Name)
	}
	if crit[b.Name] {
		return netErr(409, "Your connection to Homestack runs through %s, so it can't be removed from here.", b.Name)
	}
	var ports []NetLink
	for _, l := range st.links {
		if l.Master == b.Name && l.Group == "" {
			if l.Type != "vlan" {
				return netErr(409, "%s isn't a VLAN bridge (%s is part of it), so remove it in the file editor if you're sure.", b.Name, l.Name)
			}
			ports = append(ports, l)
		}
	}
	if len(ports) == 0 {
		return netErr(409, "%s has no VLAN in it, so it isn't a VLAN bridge. Remove it in the file editor if you're sure.", b.Name)
	}
	own := func(l NetLink) error {
		for _, f := range []string{l.NetDevFile, l.NetworkFile} {
			t, ok := st.files[f]
			if f == "" || !ok {
				return netErr(409, "%s isn't fully set up by files in %s, so remove it in the file editor.", l.Name, n.dir())
			}
			if strings.HasSuffix(f, ".network") && !slices.Equal(matchNames(t), []string{l.Name}) {
				return netErr(409, "%s sets up more than %s, so remove it in the file editor.", f, l.Name)
			}
		}
		ch.files[l.NetDevFile] = nil
		ch.files[l.NetworkFile] = nil
		return nil
	}
	if err := own(*b); err != nil {
		return err
	}
	var ids []string
	for _, v := range ports {
		if err := own(v); err != nil {
			return err
		}
		p := st.link(v.Parent)
		if p == nil || p.NetworkFile == "" {
			return netErr(409, "Can't find the file that adds %s to its parent.", v.Name)
		}
		text := st.files[p.NetworkFile]
		if t := ch.files[p.NetworkFile]; t != nil {
			text = *t
		}
		d := parseNC(text)
		found := false
		for _, s := range d.sectionsNamed("Network") {
			if i := d.lineOf(s, "VLAN", v.Name); i >= 0 {
				d.remove(i)
				found = true
				break
			}
		}
		if !found {
			return netErr(409, "%s doesn't have VLAN=%s, so remove the VLAN in the file editor.", p.NetworkFile, v.Name)
		}
		out := d.String()
		ch.files[p.NetworkFile] = &out
		ids = append(ids, strconv.Itoa(v.VLAN))
	}
	for _, a := range b.Addrs {
		if a.Scope == "global" {
			ch.notes = append(ch.notes, fmt.Sprintf("The server's own address on it, %s, goes away too.", a.Addr))
		}
	}
	ch.summary = fmt.Sprintf("Remove the VLAN %s bridge %s", strings.Join(ids, ", "), b.Name)
	return nil
}

// ---------- Applying ----------

func (n *netManager) planView(ch netChange, st netState) NetPlan {
	p := NetPlan{Summary: ch.summary, LinkOps: ch.linkOps, Notes: ch.notes, Critical: ch.critical, ConfirmWord: ch.critical, Seconds: int(netConfirmWindow.Seconds())}
	if p.Notes == nil {
		p.Notes = []string{}
	}
	for _, name := range slices.Sorted(maps.Keys(ch.files)) {
		p.Files = append(p.Files, FileChange{Name: name, Before: st.files[name], After: deref(ch.files[name])})
	}
	after := maps.Clone(st.files)
	for name, t := range ch.files {
		if t == nil {
			delete(after, name)
		} else {
			after[name] = *t
		}
	}
	p.Remove = removedDevs(st.files, after, st)
	return p
}

// removedDevs are virtual interfaces whose .netdev goes away. networkd
// doesn't delete those itself, so the agent does.
func removedDevs(before, after map[string]string, st netState) []string {
	have := map[string]bool{}
	for name, t := range after {
		if strings.HasSuffix(name, ".netdev") {
			have[netdevName(t)] = true
		}
	}
	var out []string
	for name, t := range before {
		if !strings.HasSuffix(name, ".netdev") {
			continue
		}
		dev := netdevName(t)
		if dev == "" || have[dev] {
			continue
		}
		if l := st.link(dev); l != nil && l.Type != "port" && l.Type != "loopback" {
			out = append(out, dev)
		}
	}
	sort.Strings(out)
	return out
}

func (n *netManager) apply(ctx context.Context, op NetOp) (*PendingView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending != nil {
		return nil, netErr(409, "Keep or undo the last network change first (%s).", n.pending.Summary)
	}
	ch, st, err := n.plan(ctx, op)
	if err != nil {
		return nil, err
	}
	if ch.critical != "" && strings.TrimSpace(op.ConfirmText) != ch.critical {
		return nil, netErr(400, "Type %s to confirm: this change touches the connection you're using.", ch.critical)
	}
	n.setBusy(ch.summary)
	defer n.setBusy("")

	before := n.gatewayChecks(ctx, st)
	backup, err := n.snapshot(st.files, "Before: "+ch.summary)
	if err != nil {
		return nil, netErr(500, "Couldn't save a backup of the network files first, so nothing was changed: %v", err)
	}
	since := time.Now().Add(-time.Second)
	undo := map[string]bool{}
	fail := func(stage string, err error) (*PendingView, error) {
		n.revert(context.WithoutCancel(ctx), backup, undo, st.files)
		n.event("warning", fmt.Sprintf("“%s” failed while %s, so it was undone: %v", ch.summary, stage, err))
		return nil, netErr(500, "The change failed while %s, so it was undone: %v", stage, err)
	}
	if err := n.writeFiles(ch.files); err != nil {
		return fail("writing the files", err)
	}
	if out, err := n.a.cmd(ctx, 30*time.Second, "networkctl", "reload"); err != nil {
		return fail("reloading the network", fmt.Errorf("%v %s", err, strings.TrimSpace(out)))
	}
	for _, l := range slices.Sorted(maps.Keys(ch.linkOps)) {
		up := ch.linkOps[l]
		verb := map[bool]string{true: "up", false: "down"}[up]
		if out, err := n.a.cmd(ctx, 15*time.Second, "networkctl", verb, l); err != nil {
			return fail("bringing "+l+" "+verb, fmt.Errorf("%v %s", err, strings.TrimSpace(out)))
		}
		undo[l] = !up
	}
	n.deleteDevs(ctx, removedDevs(st.files, readNetFiles(n.dir()), st))

	checks := n.waitForGateways(ctx, before)
	warnings := n.journalWarnings(ctx, since)
	if _, err := exec.LookPath("ping"); err != nil {
		warnings = append([]string{"ping isn't installed (apt install iputils-ping), so the gateways weren't tested: only the countdown protects this change."}, warnings...)
	}
	for _, c := range checks {
		if c.Before && !c.After && !n.linkStillOK(c.Link, checks) {
			n.revert(context.WithoutCancel(ctx), backup, undo, st.files)
			msg := fmt.Sprintf("After the change, %s couldn't reach its gateway %s any more, so it was undone straight away.", c.Link, c.Gateway)
			n.event("warning", "“"+ch.summary+"”: "+msg)
			return nil, netErr(409, "%s", msg)
		}
	}

	tok := make([]byte, 12)
	_, _ = rand.Read(tok)
	p := &netPending{Token: hex.EncodeToString(tok), Summary: ch.summary, Deadline: time.Now().Add(netConfirmWindow), Backup: backup, LinkUndo: undo, Warnings: warnings, Checks: checks}
	n.pending = p
	n.savePending()
	n.timer = time.AfterFunc(netConfirmWindow, func() { n.expire(p.Token) })
	n.event("ok", fmt.Sprintf("Applied “%s”, waiting to be kept.", ch.summary))
	return n.pendingView(), nil
}

// linkStillOK: a link passes if any of its gateways answers after the change.
func (n *netManager) linkStillOK(link string, checks []GatewayCheck) bool {
	for _, c := range checks {
		if c.Link == link && c.After {
			return true
		}
	}
	return false
}

func (n *netManager) pendingView() *PendingView {
	p := n.pending
	if p == nil {
		return nil
	}
	v := &PendingView{Token: p.Token, Summary: p.Summary, Seconds: max(0, int(time.Until(p.Deadline).Seconds())), Warnings: p.Warnings, Checks: p.Checks}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	if v.Checks == nil {
		v.Checks = []GatewayCheck{}
	}
	return v
}

func (n *netManager) confirm(token string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending == nil || n.pending.Token != token {
		return netErr(409, "There's no change waiting to be kept. It may already have been undone.")
	}
	n.timer.Stop()
	n.event("ok", fmt.Sprintf("Kept “%s”.", n.pending.Summary))
	n.pending = nil
	_ = os.Remove(n.pendingPath())
	return nil
}

func (n *netManager) cancel(ctx context.Context, token string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending == nil || (token != "" && n.pending.Token != token) {
		return netErr(409, "There's no change waiting. It may already have been undone.")
	}
	n.timer.Stop()
	n.undoPending(ctx, "you undid it")
	return nil
}

func (n *netManager) expire(token string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending == nil || n.pending.Token != token {
		return
	}
	n.undoPending(context.Background(), fmt.Sprintf("it wasn't kept within %d seconds", int(netConfirmWindow.Seconds())))
}

// undoPending puts the files back as they were. Callers hold n.mu.
func (n *netManager) undoPending(ctx context.Context, why string) {
	p := n.pending
	n.revert(ctx, p.Backup, p.LinkUndo, nil)
	n.pending = nil
	_ = os.Remove(n.pendingPath())
	n.event("warning", fmt.Sprintf("Undid “%s”: %s.", p.Summary, why))
}

// revert restores a backup: files, interfaces brought up or down, and
// virtual interfaces the change created.
func (n *netManager) revert(ctx context.Context, backup string, linkUndo map[string]bool, known map[string]string) {
	files, _, err := n.loadBackup(backup)
	if err != nil && known == nil {
		log.Printf("network: can't undo, backup %s unreadable: %v", backup, err)
		return
	}
	if err != nil {
		files = known
	}
	st := n.readNetState(ctx)
	change := map[string]*string{}
	for name, cur := range st.files {
		if want, ok := files[name]; !ok {
			change[name] = nil
		} else if want != cur {
			w := want
			change[name] = &w
		}
	}
	for name, want := range files {
		if _, ok := st.files[name]; !ok {
			w := want
			change[name] = &w
		}
	}
	if err := n.writeFiles(change); err != nil {
		log.Printf("network: undo couldn't write every file: %v", err)
	}
	if out, err := n.a.cmd(ctx, 30*time.Second, "networkctl", "reload"); err != nil {
		log.Printf("network: undo reload: %v %s", err, out)
	}
	for l, up := range linkUndo {
		_, _ = n.a.cmd(ctx, 15*time.Second, "networkctl", map[bool]string{true: "up", false: "down"}[up], l)
	}
	n.deleteDevs(ctx, removedDevs(st.files, files, st))
}

func (n *netManager) deleteDevs(ctx context.Context, devs []string) {
	if len(devs) == 0 {
		return
	}
	if out, err := n.a.cmd(ctx, 15*time.Second, "networkctl", append([]string{"delete"}, devs...)...); err != nil {
		log.Printf("network: deleting %v: %v %s", devs, err, out)
	}
}

// writeFiles writes (atomically, mode 0644) or deletes files in the network
// folder.
func (n *netManager) writeFiles(files map[string]*string) error {
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !reNetFile.MatchString(name) {
			return fmt.Errorf("bad file name %q", name)
		}
		p := filepath.Join(n.dir(), name)
		text := files[name]
		if text == nil {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		tmp := filepath.Join(n.dir(), "."+name+".homestack-tmp")
		if err := os.WriteFile(tmp, []byte(*text), 0o644); err != nil {
			return err
		}
		_ = os.Chmod(tmp, 0o644)
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

// ---------- Gateway checks ----------

type gwPair struct{ link, from, gw string }

func gatewayPairs(st netState) []gwPair {
	var out []gwPair
	seen := map[gwPair]bool{}
	for _, r := range st.routes {
		if r.Family != 4 || r.Gateway == "" || r.Dev == "" {
			continue
		}
		l := st.link(r.Dev)
		gw, err := netip.ParseAddr(r.Gateway)
		if l == nil || l.Group != "" || err != nil {
			continue
		}
		for _, a := range l.Addrs {
			p, err := netip.ParsePrefix(a.Addr)
			if err != nil || a.Family != 4 || a.Scope != "global" || !p.Contains(gw) {
				continue
			}
			pr := gwPair{l.Name, p.Addr().String(), gw.String()}
			if !seen[pr] {
				seen[pr] = true
				out = append(out, pr)
			}
			break
		}
	}
	return out
}

func (n *netManager) ping(ctx context.Context, pairs []gwPair) map[gwPair]bool {
	res := map[gwPair]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Go(func() {
			_, err := n.a.cmd(ctx, 4*time.Second, "ping", "-n", "-q", "-c", "1", "-W", "1", "-I", p.from, p.gw)
			mu.Lock()
			res[p] = err == nil
			mu.Unlock()
		})
	}
	wg.Wait()
	return res
}

// gatewayChecks tests each interface's gateways from the interface's own
// address (so policy routing sends each test out of the right port).
func (n *netManager) gatewayChecks(ctx context.Context, st netState) []GatewayCheck {
	pairs := gatewayPairs(st)
	res := n.ping(ctx, pairs)
	out := make([]GatewayCheck, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, GatewayCheck{Link: p.link, From: p.from, Gateway: p.gw, Before: res[p]})
	}
	return out
}

// waitForGateways re-tests after a change, giving interfaces up to 15
// seconds to come back (a bond or bridge can take a moment).
func (n *netManager) waitForGateways(ctx context.Context, before []GatewayCheck) []GatewayCheck {
	passed := map[string]bool{}
	for _, c := range before {
		if c.Before {
			passed[c.Link] = true
		}
	}
	deadline := time.Now().Add(n.checkFor)
	time.Sleep(n.settle)
	for {
		st := n.readNetState(ctx)
		pairs := gatewayPairs(st)
		res := n.ping(ctx, pairs)
		var out []GatewayCheck
		ok := map[string]bool{}
		for _, p := range pairs {
			was := false
			for _, c := range before {
				if c.Link == p.link && c.Gateway == p.gw {
					was = c.Before
				}
			}
			out = append(out, GatewayCheck{Link: p.link, From: p.from, Gateway: p.gw, Before: was, After: res[p]})
			if res[p] {
				ok[p.link] = true
			}
		}
		// Links that answered before but have no test now (their address
		// or gateway was removed on purpose) aren't failures.
		failing := false
		for l := range passed {
			has := slices.ContainsFunc(pairs, func(p gwPair) bool { return p.link == l })
			if has && !ok[l] {
				failing = true
			}
		}
		if !failing || time.Now().After(deadline) {
			return out
		}
		time.Sleep(n.settle)
	}
}

func (n *netManager) journalWarnings(ctx context.Context, since time.Time) []string {
	out, err := n.a.cmd(ctx, 10*time.Second, "journalctl", "-u", "systemd-networkd", "--since", "@"+strconv.FormatInt(since.Unix(), 10), "-p", "warning", "-o", "cat", "--no-pager", "-q")
	if err != nil {
		return []string{}
	}
	res := []string{}
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" && !slices.Contains(res, line) && len(res) < 15 {
			res = append(res, line)
		}
	}
	return res
}

// ---------- Backups ----------

func (n *netManager) backupsDir() string { return filepath.Join(n.a.opt.StateDir, "network-backups") }

func (n *netManager) pendingPath() string {
	return filepath.Join(n.a.opt.StateDir, "network-pending.json")
}

func (n *netManager) savePending() {
	b, _ := json.MarshalIndent(n.pending, "", "  ")
	if err := writeFileAtomic(n.pendingPath(), b, 0o600); err != nil {
		log.Printf("network: saving the pending change: %v", err)
	}
}

func writeFileAtomic(p string, b []byte, mode os.FileMode) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

type backupMeta struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Summary string    `json:"summary"`
}

// snapshot saves the current files. The very first one is also kept as
// "original" for good: the network as it was before Homestack changed it.
func (n *netManager) snapshot(files map[string]string, summary string) (string, error) {
	save := func(id, summary string) error {
		dir := filepath.Join(n.backupsDir(), id)
		if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
			return err
		}
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(dir, "files", name), []byte(text), 0o600); err != nil {
				return err
			}
		}
		b, _ := json.Marshal(backupMeta{ID: id, Time: time.Now(), Summary: summary})
		return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o600)
	}
	if _, err := os.Stat(filepath.Join(n.backupsDir(), "original")); errors.Is(err, os.ErrNotExist) {
		if err := save("original", "As it was before Homestack first changed the network"); err != nil {
			return "", err
		}
	}
	r := make([]byte, 2)
	_, _ = rand.Read(r)
	id := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(r)
	if err := save(id, summary); err != nil {
		return "", err
	}
	// Keep the newest few, and "original".
	list := n.backups()
	kept := 0
	for _, b := range list {
		if b.ID == "original" || b.ID == id {
			continue
		}
		if kept++; kept >= netBackupsKept {
			_ = os.RemoveAll(filepath.Join(n.backupsDir(), b.ID))
		}
	}
	return id, nil
}

func (n *netManager) backups() []NetBackup {
	des, _ := os.ReadDir(n.backupsDir())
	out := []NetBackup{}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		var m backupMeta
		b, err := os.ReadFile(filepath.Join(n.backupsDir(), de.Name(), "meta.json"))
		if err != nil || json.Unmarshal(b, &m) != nil {
			continue
		}
		fs, _ := os.ReadDir(filepath.Join(n.backupsDir(), de.Name(), "files"))
		out = append(out, NetBackup{ID: de.Name(), Time: m.Time, Summary: m.Summary, Files: len(fs)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

func (n *netManager) loadBackup(id string) (map[string]string, backupMeta, error) {
	var m backupMeta
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return nil, m, netErr(400, "Unknown backup.")
	}
	dir := filepath.Join(n.backupsDir(), id)
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return nil, m, netErr(404, "That backup doesn't exist any more.")
	}
	files := readNetFiles(filepath.Join(dir, "files"))
	return files, m, nil
}

// recover undoes a change that was never kept, after the agent restarted
// (or before it stops): nobody can confirm it any more.
func (n *netManager) recover(ctx context.Context, why string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending == nil {
		b, err := os.ReadFile(n.pendingPath())
		if err != nil {
			return
		}
		var p netPending
		if json.Unmarshal(b, &p) != nil {
			_ = os.Remove(n.pendingPath())
			return
		}
		n.pending = &p
	}
	if n.timer != nil {
		n.timer.Stop()
	}
	n.undoPending(ctx, why)
}

// RestoreNetwork undoes an unconfirmed network change (systemd runs it when
// the agent stops).
func RestoreNetwork(opt Options) error {
	a := New(opt)
	a.net.recover(context.Background(), "the host agent stopped before it was kept")
	return nil
}

// ---------- HTTP ----------

func (n *netManager) view(ctx context.Context, via string) NetView {
	st := n.readNetState(ctx)
	yours, crit := st.critical(via)
	v := NetView{Managed: st.managed, Note: st.note, Dir: n.dir(), Links: st.links, Routes: st.routes, Rules: st.rules, DNS: st.dns, Backups: n.backups(), Window: int(netConfirmWindow.Seconds())}
	for i := range v.Links {
		v.Links[i].Yours = v.Links[i].Name == yours
		v.Links[i].Critical = crit[v.Links[i].Name]
	}
	for _, name := range slices.Sorted(maps.Keys(st.files)) {
		f := NetFile{Name: name, Type: strings.TrimPrefix(path.Ext(name), "."), Size: len(st.files[name]), Links: []string{}}
		for _, l := range st.links {
			if l.NetworkFile == name || l.NetDevFile == name {
				f.Links = append(f.Links, l.Name)
			}
		}
		v.Files = append(v.Files, f)
	}
	if v.Files == nil {
		v.Files = []NetFile{}
	}
	if v.DNS == nil {
		v.DNS = []string{}
	}
	if v.Links == nil {
		v.Links = []NetLink{}
	}
	if v.Routes == nil {
		v.Routes = []NetRoute{}
	}
	if v.Rules == nil {
		v.Rules = []NetRule{}
	}
	if n.mu.TryLock() {
		v.Pending = n.pendingView()
		n.mu.Unlock()
	}
	n.stmu.Lock()
	v.Busy, v.Last = n.busy, n.last
	n.stmu.Unlock()
	return v
}

func netWriteErr(w http.ResponseWriter, err error) {
	if ne, ok := errors.AsType[*netError](err); ok {
		writeErr(w, ne.status, ne.msg)
		return
	}
	writeErr(w, 500, err.Error())
}

func (n *netManager) getView(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, n.view(r.Context(), r.URL.Query().Get("via")))
}

func (n *netManager) getFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !reNetFile.MatchString(name) {
		writeErr(w, 400, "bad file name")
		return
	}
	b, err := os.ReadFile(filepath.Join(n.dir(), name))
	if err != nil {
		writeErr(w, 404, "There's no "+name+".")
		return
	}
	writeJSON(w, 200, map[string]string{"name": name, "content": string(b)})
}

func (n *netManager) postPlan(w http.ResponseWriter, r *http.Request) {
	var op NetOp
	if err := readBody(r, &op); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	ch, st, err := n.plan(r.Context(), op)
	if err != nil {
		netWriteErr(w, err)
		return
	}
	writeJSON(w, 200, n.planView(ch, st))
}

func (n *netManager) postApply(w http.ResponseWriter, r *http.Request) {
	var op NetOp
	if err := readBody(r, &op); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	p, err := n.apply(r.Context(), op)
	if err != nil {
		netWriteErr(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (n *netManager) postConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	_ = readBody(r, &req)
	if err := n.confirm(req.Token); err != nil {
		netWriteErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (n *netManager) postCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	_ = readBody(r, &req)
	if err := n.cancel(context.WithoutCancel(r.Context()), req.Token); err != nil {
		netWriteErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
