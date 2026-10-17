package agent

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// systemd-networkd's .network, .netdev and .link files, edited without
// losing anything: comments, blank lines, order and every setting Homestack
// doesn't know about stay exactly as they were. Only the lines a change is
// about are touched.

type ncDoc struct{ lines []string }

type ncSection struct {
	name       string
	start, end int // header line; one past the section's last line
}

func parseNC(text string) *ncDoc {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return &ncDoc{}
	}
	return &ncDoc{lines: strings.Split(text, "\n")}
}

func (d *ncDoc) String() string {
	if len(d.lines) == 0 {
		return ""
	}
	return strings.Join(d.lines, "\n") + "\n"
}

func ncHeader(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if len(t) > 2 && t[0] == '[' && t[len(t)-1] == ']' {
		return t[1 : len(t)-1], true
	}
	return "", false
}

func ncKV(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || t[0] == '#' || t[0] == ';' || t[0] == '[' {
		return "", "", false
	}
	k, v, ok := strings.Cut(t, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

func (d *ncDoc) sections() []ncSection {
	var out []ncSection
	for i, l := range d.lines {
		if name, ok := ncHeader(l); ok {
			if n := len(out); n > 0 {
				out[n-1].end = i
			}
			out = append(out, ncSection{name: name, start: i, end: len(d.lines)})
		}
	}
	return out
}

func (d *ncDoc) sectionsNamed(name string) []ncSection {
	var out []ncSection
	for _, s := range d.sections() {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

// get returns the values of key in one section.
func (d *ncDoc) get(s ncSection, key string) []string {
	var out []string
	for i := s.start + 1; i < s.end; i++ {
		if k, v, ok := ncKV(d.lines[i]); ok && k == key {
			out = append(out, v)
		}
	}
	return out
}

func (d *ncDoc) get1(s ncSection, key string) string {
	v := d.get(s, key)
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1] // the last one wins, as in networkd
}

// values returns key's values across every section called name.
func (d *ncDoc) values(name, key string) []string {
	var out []string
	for _, s := range d.sectionsNamed(name) {
		out = append(out, d.get(s, key)...)
	}
	return out
}

// lineOf finds the first line in s setting key (to value, if value != "").
func (d *ncDoc) lineOf(s ncSection, key, value string) int {
	for i := s.start + 1; i < s.end; i++ {
		if k, v, ok := ncKV(d.lines[i]); ok && k == key && (value == "" || v == value) {
			return i
		}
	}
	return -1
}

// ensure returns the first section called name, adding it at the end.
func (d *ncDoc) ensure(name string) ncSection {
	if ss := d.sectionsNamed(name); len(ss) > 0 {
		return ss[0]
	}
	if n := len(d.lines); n > 0 && strings.TrimSpace(d.lines[n-1]) != "" {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, "["+name+"]")
	return d.sectionsNamed(name)[0]
}

// add puts Key=Value after the last setting in s.
func (d *ncDoc) add(s ncSection, key, value string) {
	at := s.start + 1
	for i := s.start + 1; i < s.end; i++ {
		if strings.TrimSpace(d.lines[i]) != "" {
			at = i + 1
		}
	}
	d.lines = slices.Insert(d.lines, at, key+"="+value)
}

func (d *ncDoc) setLine(i int, key, value string) {
	indent := d.lines[i][:len(d.lines[i])-len(strings.TrimLeft(d.lines[i], " \t"))]
	d.lines[i] = indent + key + "=" + value
}

func (d *ncDoc) remove(i int) { d.lines = slices.Delete(d.lines, i, i+1) }

// removeSection drops a section and the blank line before it.
func (d *ncDoc) removeSection(s ncSection) {
	start := s.start
	if start > 0 && strings.TrimSpace(d.lines[start-1]) == "" {
		start--
	}
	d.lines = slices.Delete(d.lines, start, s.end)
}

// ---------- Validation ----------

var (
	reNetFile  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,60}\.(network|netdev|link)$`)
	reIfName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$`)
	reSection  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	reKey      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	netdevKind = []string{"bond", "bridge", "vlan", "dummy", "macvlan", "ipvlan", "macvtap", "ipvtap", "veth", "wireguard", "vxlan", "tun", "tap", "vrf", "geneve", "gre", "sit", "ipip"}
)

const maxNetFile = 64 << 10

// validateNetFile checks a file is well formed, and refuses the mistakes
// that take a whole server off the network: a .network file without a
// [Match] (it would claim every interface, Docker's and the VMs' included)
// or a .netdev without a name and kind.
func validateNetFile(name, text string) error {
	if !reNetFile.MatchString(name) {
		return fmt.Errorf("%q isn't a valid file name: use letters, digits and . _ @ -, ending in .network, .netdev or .link", name)
	}
	if len(text) > maxNetFile {
		return fmt.Errorf("%s is too big (the limit is 64 KB)", name)
	}
	d := parseNC(text)
	section, cont := "", false
	for i, l := range d.lines {
		t := strings.TrimSpace(l)
		if cont {
			cont = strings.HasSuffix(t, "\\")
			continue
		}
		if t == "" || t[0] == '#' || t[0] == ';' {
			continue
		}
		if h, ok := ncHeader(t); ok {
			if !reSection.MatchString(h) {
				return fmt.Errorf("%s line %d: %q isn't a section name", name, i+1, h)
			}
			section = h
			continue
		}
		k, _, ok := ncKV(t)
		if !ok || !reKey.MatchString(k) {
			return fmt.Errorf("%s line %d: expected Key=Value or [Section], found %q", name, i+1, t)
		}
		if section == "" {
			return fmt.Errorf("%s line %d: %s= comes before any [Section]", name, i+1, k)
		}
		cont = strings.HasSuffix(t, "\\")
	}
	switch path.Ext(name) {
	case ".network", ".link":
		ms := d.sectionsNamed("Match")
		has := false
		for _, s := range ms {
			for i := s.start + 1; i < s.end; i++ {
				if _, v, ok := ncKV(d.lines[i]); ok && v != "" {
					has = true
				}
			}
		}
		if !has {
			return fmt.Errorf("%s needs a [Match] section saying which interface it's for (Name=…). Without one it would apply to every interface, including Docker's and the VMs'", name)
		}
	case ".netdev":
		ss := d.sectionsNamed("NetDev")
		if len(ss) == 0 {
			return fmt.Errorf("%s needs a [NetDev] section with Name= and Kind=", name)
		}
		n, k := d.get1(ss[0], "Name"), d.get1(ss[0], "Kind")
		if !reIfName.MatchString(n) {
			return fmt.Errorf("%s: Name=%q isn't a valid interface name (up to 15 letters, digits and . _ -)", name, n)
		}
		if !slices.Contains(netdevKind, k) {
			return fmt.Errorf("%s: Kind=%q isn't a kind of device networkd creates", name, k)
		}
	}
	return nil
}

// netdevName is the interface a .netdev file creates.
func netdevName(text string) string {
	d := parseNC(text)
	if ss := d.sectionsNamed("NetDev"); len(ss) > 0 {
		return d.get1(ss[0], "Name")
	}
	return ""
}

// matchNames are the Name= patterns in a .network file's [Match].
func matchNames(text string) []string {
	var out []string
	for _, v := range parseNC(text).values("Match", "Name") {
		out = append(out, strings.Fields(v)...)
	}
	return out
}

// ---------- The settings the form edits ----------

// LinkConfig is what the Edit form shows for one interface's .network file.
type LinkConfig struct {
	File      string   `json:"file"`
	DHCP      string   `json:"dhcp"` // no | yes | ipv4 | ipv6
	Addresses []string `json:"addresses"`
	Gateway4  string   `json:"gateway4"`
	Gateway6  string   `json:"gateway6"`
	DNS       []string `json:"dns"`
	MTU       int      `json:"mtu,omitempty"`
	Port      string   `json:"port,omitempty"`   // "bridge br0" / "bond bond0": a port carries no addresses
	VLANs     []string `json:"vlans,omitempty"`  // VLAN= on this link
	Policy    int      `json:"policy,omitempty"` // routes in other tables and routing rules, edited in the file
}

func isDefaultDst(dst string) bool {
	return dst == "" || dst == "0.0.0.0/0" || dst == "::/0" || dst == "default"
}

func isMainTable(t string) bool { return t == "" || t == "main" || t == "254" }

func readLinkConfig(file, text string) LinkConfig {
	d := parseNC(text)
	c := LinkConfig{File: file, DHCP: "no", Addresses: []string{}, DNS: []string{}}
	if v := d.values("Network", "DHCP"); len(v) > 0 {
		c.DHCP = v[len(v)-1]
	}
	c.Addresses = append(c.Addresses, d.values("Network", "Address")...)
	c.Addresses = append(c.Addresses, d.values("Address", "Address")...)
	for _, g := range d.values("Network", "Gateway") {
		c.setGateway(g)
	}
	for _, s := range d.sectionsNamed("Route") {
		if !isMainTable(d.get1(s, "Table")) {
			c.Policy++
			continue
		}
		if g := d.get1(s, "Gateway"); g != "" && isDefaultDst(d.get1(s, "Destination")) {
			c.setGateway(g)
		}
	}
	c.Policy += len(d.sectionsNamed("RoutingPolicyRule"))
	for _, v := range d.values("Network", "DNS") {
		c.DNS = append(c.DNS, strings.Fields(v)...)
	}
	if v := d.values("Link", "MTUBytes"); len(v) > 0 {
		c.MTU, _ = strconv.Atoi(v[len(v)-1])
	}
	if v := d.values("Network", "Bridge"); len(v) > 0 {
		c.Port = "bridge " + v[len(v)-1]
	} else if v := d.values("Network", "Bond"); len(v) > 0 {
		c.Port = "bond " + v[len(v)-1]
	}
	c.VLANs = d.values("Network", "VLAN")
	return c
}

func (c *LinkConfig) setGateway(g string) {
	a, err := netip.ParseAddr(g)
	switch {
	case err != nil:
	case a.Is4() && c.Gateway4 == "":
		c.Gateway4 = g
	case a.Is6() && c.Gateway6 == "":
		c.Gateway6 = g
	}
}

// AddrEdit is one address in the form: Was is what the file has now ("" for
// a new one), Now what it should be. Addresses the form doesn't mention are
// removed.
type AddrEdit struct {
	Was string `json:"was"`
	Now string `json:"now"`
}

// LinkConfigEdit is the form's answer.
type LinkConfigEdit struct {
	DHCP      string     `json:"dhcp"`
	Addresses []AddrEdit `json:"addresses"`
	Gateway4  string     `json:"gateway4"`
	Gateway6  string     `json:"gateway6"`
	DNS       []string   `json:"dns"`
	MTU       int        `json:"mtu"`
}

func validCIDR(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return p, fmt.Errorf("%q isn't an address with a prefix length, like 10.0.10.5/24", s)
	}
	a := p.Addr()
	if a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
		return p, fmt.Errorf("%s can't be an interface address", s)
	}
	if a.Is4() && p.Bits() < 31 {
		if a == p.Masked().Addr() {
			return p, fmt.Errorf("%s is the network's own address, not one a machine can use", s)
		}
		if a == lastAddr(p) {
			return p, fmt.Errorf("%s is the network's broadcast address, not one a machine can use", s)
		}
	}
	if a.Is4() && p.Bits() < 8 || a.Is6() && p.Bits() < 16 {
		return p, fmt.Errorf("%s: a /%d network is far too big; check the prefix length", s, p.Bits())
	}
	return p, nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	bits := p.Bits()
	for i := range b {
		for j := 0; j < 8; j++ {
			if i*8+j >= bits {
				b[i] |= 0x80 >> j
			}
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// applyLinkConfig edits a .network file to match the form. It returns the
// new text and notes for the review screen.
func applyLinkConfig(text string, e LinkConfigEdit) (string, []string, error) {
	d := parseNC(text)
	cur := readLinkConfig("", text)
	var notes []string

	switch e.DHCP {
	case "", "no", "yes", "ipv4", "ipv6":
	default:
		return "", nil, fmt.Errorf("DHCP must be no, yes, ipv4 or ipv6")
	}
	if e.DHCP == "" {
		e.DHCP = "no"
	}
	if cur.Port != "" {
		live := 0
		for _, a := range e.Addresses {
			if a.Now != "" {
				live++
			}
		}
		if live > 0 || e.DHCP != "no" || e.Gateway4 != "" || e.Gateway6 != "" {
			return "", nil, fmt.Errorf("this interface is a port of %s, so it can't have its own addresses or gateway; set them on %s instead", strings.Fields(cur.Port)[1], strings.Fields(cur.Port)[1])
		}
	}

	// Addresses: validate first, then edit in place, so [Address] sections
	// keep their other settings (RouteMetric and so on).
	var want []netip.Prefix
	for _, a := range e.Addresses {
		if a.Now == "" {
			continue
		}
		p, err := validCIDR(a.Now)
		if err != nil {
			return "", nil, err
		}
		for _, w := range want {
			if w.Addr() == p.Addr() {
				return "", nil, fmt.Errorf("%s is listed twice", p.Addr())
			}
			if w.Masked() != p.Masked() && (w.Contains(p.Addr()) || p.Contains(w.Addr())) {
				return "", nil, fmt.Errorf("%s and %s overlap but have different prefix lengths", w, p)
			}
		}
		want = append(want, p)
	}
	kept := map[string]string{} // was -> now
	for _, a := range e.Addresses {
		if a.Was != "" {
			kept[a.Was] = a.Now
		}
	}
	// Decide every line's fate against the file as it is, then edit from
	// the bottom up so earlier line numbers stay valid.
	type addrAct struct {
		i       int
		sec     ncSection
		now     string
		section bool // remove the whole [Address] section
	}
	var acts []addrAct
	for _, s := range d.sections() {
		if s.name != "Network" && s.name != "Address" {
			continue
		}
		for i := s.start + 1; i < s.end; i++ {
			k, v, ok := ncKV(d.lines[i])
			if !ok || k != "Address" {
				continue
			}
			now, mentioned := kept[v]
			if mentioned && now == v {
				continue
			}
			acts = append(acts, addrAct{i: i, sec: s, now: now, section: s.name == "Address"})
		}
	}
	for j := len(acts) - 1; j >= 0; j-- {
		a := acts[j]
		switch {
		case a.now != "":
			d.setLine(a.i, "Address", a.now)
		case a.section:
			d.removeSection(a.sec)
		default:
			d.remove(a.i)
		}
	}
	for _, a := range e.Addresses {
		if a.Was == "" && a.Now != "" {
			d.add(d.ensure("Network"), "Address", a.Now)
		}
	}

	// DHCP
	if e.DHCP != cur.DHCP {
		s := d.ensure("Network")
		if i := d.lineOf(s, "DHCP", ""); i >= 0 {
			if e.DHCP == "no" {
				d.remove(i)
			} else {
				d.setLine(i, "DHCP", e.DHCP)
			}
		} else if e.DHCP != "no" {
			d.add(s, "DHCP", e.DHCP)
		}
	}

	// Gateways
	for _, g := range []struct {
		old, now string
		v6       bool
	}{{cur.Gateway4, e.Gateway4, false}, {cur.Gateway6, e.Gateway6, true}} {
		if g.old == g.now {
			continue
		}
		if g.now != "" {
			a, err := netip.ParseAddr(g.now)
			if err != nil || a.Is6() != g.v6 {
				return "", nil, fmt.Errorf("%q isn't an IPv%s address", g.now, map[bool]string{false: "4", true: "6"}[g.v6])
			}
			if !g.v6 || !a.IsLinkLocalUnicast() {
				in := false
				for _, p := range want {
					if p.Contains(a) {
						in = true
					}
				}
				if !in && e.DHCP == "no" {
					return "", nil, fmt.Errorf("the gateway %s isn't in any of this interface's networks, so it couldn't be reached", g.now)
				}
			}
		}
		d.changeGateway(g.old, g.now, &notes)
	}

	// DNS
	if !slices.Equal(e.DNS, cur.DNS) {
		var v4, v6 []string
		for _, s := range e.DNS {
			a, err := netip.ParseAddr(strings.TrimSpace(s))
			if err != nil {
				return "", nil, fmt.Errorf("%q isn't a DNS server address", s)
			}
			if a.Is4() {
				v4 = append(v4, a.String())
			} else {
				v6 = append(v6, a.String())
			}
		}
		s := d.ensure("Network")
		at := -1
		for i := s.end - 1; i > s.start; i-- {
			if k, _, ok := ncKV(d.lines[i]); ok && k == "DNS" {
				d.remove(i)
				at = i
			}
		}
		lines := []string{}
		if len(v4) > 0 {
			lines = append(lines, "DNS="+strings.Join(v4, " "))
		}
		if len(v6) > 0 {
			lines = append(lines, "DNS="+strings.Join(v6, " "))
		}
		if at >= 0 {
			d.lines = slices.Insert(d.lines, at, lines...)
		} else {
			s = d.ensure("Network")
			for _, l := range lines {
				k, v, _ := ncKV(l)
				d.add(s, k, v)
				s = d.sectionsNamed("Network")[0]
			}
		}
	}

	// MTU
	if e.MTU != cur.MTU {
		if e.MTU != 0 && (e.MTU < 1280 || e.MTU > 9216) {
			return "", nil, fmt.Errorf("MTU must be between 1280 and 9216 (1500 is standard; 9000 is jumbo frames)")
		}
		ls := d.sectionsNamed("Link")
		i := -1
		if len(ls) > 0 {
			i = d.lineOf(ls[0], "MTUBytes", "")
		}
		switch {
		case e.MTU == 0 && i >= 0:
			d.remove(i)
		case e.MTU != 0 && i >= 0:
			d.setLine(i, "MTUBytes", strconv.Itoa(e.MTU))
		case e.MTU != 0:
			d.add(d.ensure("Link"), "MTUBytes", strconv.Itoa(e.MTU))
		}
		if e.MTU > 1500 {
			notes = append(notes, "An MTU above 1500 (jumbo frames) only works if every switch and device on the path supports it, and ports of a bond or bridge need the same MTU.")
		}
	}
	return d.String(), notes, nil
}

// changeGateway sets the default gateway. A new gateway replaces the old
// one everywhere in the file, policy routing tables included, so they keep
// matching; removing it only drops the main table's default route (other
// routes and policy tables are left for the file editor).
func (d *ncDoc) changeGateway(old, now string, notes *[]string) {
	switch {
	case old == "" && now != "":
		d.add(d.ensure("Network"), "Gateway", now)
	case old != "" && now == "":
		for removed := true; removed; {
			removed = false
			for _, s := range d.sections() {
				if s.name == "Network" {
					if i := d.lineOf(s, "Gateway", old); i >= 0 {
						d.remove(i)
						removed = true
						break
					}
				}
				if s.name == "Route" && isMainTable(d.get1(s, "Table")) && isDefaultDst(d.get1(s, "Destination")) && d.get1(s, "Gateway") == old {
					d.removeSection(s)
					removed = true
					break
				}
			}
		}
	case old != "":
		other := 0
		for _, s := range d.sections() { // setLine keeps line numbers, so s stays valid
			if s.name != "Network" && s.name != "Route" {
				continue
			}
			for i := s.start + 1; i < s.end; i++ {
				if k, v, ok := ncKV(d.lines[i]); ok && k == "Gateway" && v == old {
					d.setLine(i, "Gateway", now)
					if s.name == "Route" && !isMainTable(d.get1(s, "Table")) {
						other++
					}
				}
			}
		}
		if other > 0 {
			*notes = append(*notes, fmt.Sprintf("The gateway also changes in %d policy routing route(s) in this file, so they keep matching.", other))
		}
	}
}

var errNoChange = errors.New("nothing to change: that's how it's set already")
