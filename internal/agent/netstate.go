package agent

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reading the host's network: interfaces, addresses, routes, routing rules
// and DNS from iproute2's JSON (`ip -j`), which file configures each
// interface from systemd-networkd, and link speeds from /sys.

// NetAddr is one address on an interface.
type NetAddr struct {
	Addr    string `json:"addr"`   // 10.0.10.5/24
	Family  int    `json:"family"` // 4 | 6
	Scope   string `json:"scope"`  // global | link | host
	Dynamic bool   `json:"dynamic,omitempty"`
}

// NetLink is one network interface.
type NetLink struct {
	Name        string      `json:"name"`
	Index       int         `json:"index"`
	Type        string      `json:"type"`            // port | bond | bridge | vlan | wireguard | tunnel | macvlan | dummy | veth | loopback | other
	Group       string      `json:"group,omitempty"` // docker | vm: virtual interfaces those make, hidden by default
	Driver      string      `json:"driver,omitempty"`
	MAC         string      `json:"mac,omitempty"`
	MTU         int         `json:"mtu"`
	Up          bool        `json:"up"`      // switched on
	Carrier     bool        `json:"carrier"` // has a link (cable, or the device below is up)
	Oper        string      `json:"oper"`
	Speed       int         `json:"speed,omitempty"` // Mb/s
	Duplex      string      `json:"duplex,omitempty"`
	Master      string      `json:"master,omitempty"` // the bridge or bond it's part of
	Parent      string      `json:"parent,omitempty"` // a VLAN's underlying interface
	VLAN        int         `json:"vlan,omitempty"`
	BondMode    string      `json:"bondMode,omitempty"`
	Active      string      `json:"active,omitempty"`    // a bond's active port
	PortState   string      `json:"portState,omitempty"` // in a bond: active | backup; in a bridge: forwarding…
	Addrs       []NetAddr   `json:"addrs"`
	DNS         []string    `json:"dns,omitempty"`
	NetworkFile string      `json:"networkFile,omitempty"` // a name in /etc/systemd/network, or a full path elsewhere (read-only)
	NetDevFile  string      `json:"netdevFile,omitempty"`
	Setup       string      `json:"setup,omitempty"` // networkd: configured, unmanaged, failed, pending…
	Config      *LinkConfig `json:"config,omitempty"`
	Yours       bool        `json:"yours,omitempty"`    // you reach Homestack through it
	Critical    bool        `json:"critical,omitempty"` // your connection depends on it

	permMAC string // a bond port's own MAC (its current one is the bond's)
}

// NetRoute is one route.
type NetRoute struct {
	Family   int    `json:"family"`
	Dst      string `json:"dst"`
	Gateway  string `json:"gateway,omitempty"`
	Dev      string `json:"dev,omitempty"`
	Table    string `json:"table"`
	Metric   int    `json:"metric,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Src      string `json:"src,omitempty"`
	Type     string `json:"type,omitempty"`
}

// NetRule is one policy routing rule.
type NetRule struct {
	Family   int    `json:"family"`
	Priority int    `json:"priority"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	IIF      string `json:"iif,omitempty"`
	OIF      string `json:"oif,omitempty"`
	Table    string `json:"table"`
	Suppress *int   `json:"suppress,omitempty"` // SuppressPrefixLength
}

// NetFile is one file in /etc/systemd/network.
type NetFile struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"` // network | netdev | link
	Links []string `json:"links"`
	Size  int      `json:"size"`
}

type netState struct {
	managed bool
	note    string
	links   []NetLink
	routes  []NetRoute
	rules   []NetRule
	dns     []string
	files   map[string]string // name -> content, in NetDir
}

func (n *netManager) dir() string { return n.a.opt.NetDir }

// readFiles reads the top-level config files in /etc/systemd/network.
// Drop-in folders (x.network.d/) aren't edited by Homestack, so they're
// neither read nor touched.
func readNetFiles(dir string) map[string]string {
	out := map[string]string{}
	des, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, de := range des {
		if !de.Type().IsRegular() {
			continue
		}
		switch path.Ext(de.Name()) {
		case ".network", ".netdev", ".link":
			if b, err := os.ReadFile(filepath.Join(dir, de.Name())); err == nil && len(b) <= maxNetFile {
				out[de.Name()] = string(b)
			}
		}
	}
	return out
}

type ipLinkJSON struct {
	Ifindex   int      `json:"ifindex"`
	Ifname    string   `json:"ifname"`
	Link      string   `json:"link"`
	Flags     []string `json:"flags"`
	MTU       int      `json:"mtu"`
	Master    string   `json:"master"`
	Operstate string   `json:"operstate"`
	LinkType  string   `json:"link_type"`
	Address   string   `json:"address"`
	Permaddr  string   `json:"permaddr"`
	Linkinfo  *struct {
		Kind      string          `json:"info_kind"`
		Data      json.RawMessage `json:"info_data"`
		SlaveKind string          `json:"info_slave_kind"`
		SlaveData json.RawMessage `json:"info_slave_data"`
	} `json:"linkinfo"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
		Dynamic   bool   `json:"dynamic"`
	} `json:"addr_info"`
}

var (
	reDockerIf = regexp.MustCompile(`^(veth[0-9a-f]+|docker0|docker_gwbridge|br-[0-9a-f]{12})$`)
	reVMIf     = regexp.MustCompile(`^(vnet\d+|virbr\d+(-nic)?|macvtap\d+|tap\d+)$`)
)

func linkGroup(name string) string {
	switch {
	case reDockerIf.MatchString(name):
		return "docker"
	case reVMIf.MatchString(name):
		return "vm"
	}
	return ""
}

// readNetState gathers everything about the host's network.
func (n *netManager) readNetState(ctx context.Context) netState {
	a := n.a
	st := netState{files: readNetFiles(n.dir())}
	out, err := a.cmd(ctx, 5*time.Second, "systemctl", "is-active", "systemd-networkd")
	st.managed = err == nil && strings.TrimSpace(out) == "active"
	if !st.managed {
		st.note = "systemd-networkd isn't running, so Homestack can show the network but not change it."
	}

	// Interfaces and addresses
	var raws []json.RawMessage
	if out, err := a.cmd(ctx, 5*time.Second, "ip", "-j", "-d", "addr", "show"); err == nil {
		_ = json.Unmarshal([]byte(out), &raws)
	}
	for _, raw := range raws {
		var l ipLinkJSON
		if json.Unmarshal(raw, &l) != nil || l.Ifname == "" {
			continue
		}
		st.links = append(st.links, n.toLink(l))
	}

	// Which file configures what, from networkd itself
	if st.managed {
		if out, err := a.cmd(ctx, 5*time.Second, "networkctl", "--json=short", "list"); err == nil {
			var v struct {
				Interfaces []struct {
					Name                string
					AdministrativeState string
					NetworkFile         string
				}
			}
			if json.Unmarshal([]byte(out), &v) == nil {
				for _, i := range v.Interfaces {
					for k := range st.links {
						if st.links[k].Name == i.Name {
							st.links[k].Setup = i.AdministrativeState
							st.links[k].NetworkFile = n.relFile(i.NetworkFile)
						}
					}
				}
			}
		}
	}
	for name, text := range st.files {
		if strings.HasSuffix(name, ".netdev") {
			if dev := netdevName(text); dev != "" {
				for k := range st.links {
					if st.links[k].Name == dev {
						st.links[k].NetDevFile = name
					}
				}
			}
		}
	}
	for k := range st.links {
		l := &st.links[k]
		if text, ok := st.files[l.NetworkFile]; ok && l.NetworkFile != "" {
			c := readLinkConfig(l.NetworkFile, text)
			l.Config = &c
		}
	}

	// DNS
	if out, err := a.cmd(ctx, 5*time.Second, "resolvectl", "dns"); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			head, rest, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			servers := strings.Fields(rest)
			if head == "Global" {
				st.dns = servers
				continue
			}
			// Link 4 (br0)
			if i, j := strings.Index(head, "("), strings.LastIndex(head, ")"); i > 0 && j > i {
				name := head[i+1 : j]
				for k := range st.links {
					if st.links[k].Name == name {
						st.links[k].DNS = servers
					}
				}
			}
		}
	}

	st.routes = n.readRoutes(ctx)
	st.rules = n.readRules(ctx)
	return st
}

// relFile shortens files in /etc/systemd/network to their name; others
// (like /usr/lib/systemd/network/…) keep their path and stay read-only.
func (n *netManager) relFile(p string) string {
	if p == "" {
		return ""
	}
	if filepath.Dir(p) == filepath.Clean(n.dir()) {
		return filepath.Base(p)
	}
	return p
}

func (n *netManager) toLink(l ipLinkJSON) NetLink {
	sys := n.a.sys("/sys/class/net/" + l.Ifname)
	nl := NetLink{
		Name: l.Ifname, Index: l.Ifindex, MTU: l.MTU, MAC: l.Address, Master: l.Master, Parent: l.Link,
		Oper: strings.ToLower(l.Operstate), Addrs: []NetAddr{}, Group: linkGroup(l.Ifname),
		Up: slices.Contains(l.Flags, "UP"), Carrier: slices.Contains(l.Flags, "LOWER_UP"),
	}
	nl.permMAC = l.Permaddr
	kind := ""
	if l.Linkinfo != nil {
		kind = l.Linkinfo.Kind
		switch kind {
		case "vlan":
			var d struct {
				ID int `json:"id"`
			}
			_ = json.Unmarshal(l.Linkinfo.Data, &d)
			nl.VLAN = d.ID
		case "bond":
			var d struct {
				Mode   string `json:"mode"`
				Active string `json:"active_slave"`
			}
			_ = json.Unmarshal(l.Linkinfo.Data, &d)
			nl.BondMode, nl.Active = d.Mode, d.Active
		}
		var sd struct {
			State string `json:"state"`
			Perm  string `json:"perm_hwaddr"`
		}
		if len(l.Linkinfo.SlaveData) > 0 && json.Unmarshal(l.Linkinfo.SlaveData, &sd) == nil {
			nl.PortState = strings.ToLower(sd.State)
			if sd.Perm != "" {
				nl.permMAC = sd.Perm
			}
		}
	}
	_, statErr := os.Stat(filepath.Join(sys, "device"))
	physical := statErr == nil
	switch {
	case l.LinkType == "loopback":
		nl.Type = "loopback"
	case kind == "bond", kind == "bridge", kind == "vlan", kind == "wireguard", kind == "dummy", kind == "veth", kind == "macvlan", kind == "ipvlan":
		nl.Type = kind
	case kind == "tun", kind == "tap", l.LinkType == "none":
		nl.Type = "tunnel"
	case physical:
		nl.Type = "port"
	default:
		nl.Type = "other"
	}
	if physical {
		if t, err := os.Readlink(filepath.Join(sys, "device", "driver")); err == nil {
			nl.Driver = filepath.Base(t)
		}
		if nl.Carrier {
			if b, err := os.ReadFile(filepath.Join(sys, "speed")); err == nil {
				if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
					nl.Speed = v
				}
			}
			if b, err := os.ReadFile(filepath.Join(sys, "duplex")); err == nil {
				nl.Duplex = strings.TrimSpace(string(b))
			}
		}
	}
	for _, ai := range l.AddrInfo {
		fam := 4
		if ai.Family == "inet6" {
			fam = 6
		}
		nl.Addrs = append(nl.Addrs, NetAddr{Addr: ai.Local + "/" + strconv.Itoa(ai.Prefixlen), Family: fam, Scope: ai.Scope, Dynamic: ai.Dynamic})
	}
	return nl
}

func (n *netManager) readRoutes(ctx context.Context) []NetRoute {
	var out []NetRoute
	for _, fam := range []int{4, 6} {
		// Without -4, "table all" lists IPv6 routes too.
		args := []string{"-" + strconv.Itoa(fam), "-j", "route", "show", "table", "all"}
		s, err := n.a.cmd(ctx, 5*time.Second, "ip", args...)
		if err != nil {
			continue
		}
		var rs []struct {
			Type     string `json:"type"`
			Dst      string `json:"dst"`
			Gateway  string `json:"gateway"`
			Dev      string `json:"dev"`
			Table    string `json:"table"`
			Metric   int    `json:"metric"`
			Protocol string `json:"protocol"`
			Prefsrc  string `json:"prefsrc"`
		}
		if json.Unmarshal([]byte(s), &rs) != nil {
			continue
		}
		for _, r := range rs {
			if r.Table == "local" || (r.Type != "" && r.Type != "unicast" && r.Type != "blackhole" && r.Type != "unreachable" && r.Type != "prohibit") {
				continue
			}
			if fam == 6 && (strings.HasPrefix(r.Dst, "fe80::") || strings.HasPrefix(r.Dst, "ff00::")) {
				continue
			}
			if r.Table == "" {
				r.Table = "main"
			}
			out = append(out, NetRoute{Family: fam, Dst: r.Dst, Gateway: r.Gateway, Dev: r.Dev, Table: r.Table, Metric: r.Metric, Protocol: r.Protocol, Src: r.Prefsrc, Type: r.Type})
		}
	}
	return out
}

func (n *netManager) readRules(ctx context.Context) []NetRule {
	var out []NetRule
	for _, fam := range []int{4, 6} {
		args := []string{"-" + strconv.Itoa(fam), "-j", "rule", "show"}
		s, err := n.a.cmd(ctx, 5*time.Second, "ip", args...)
		if err != nil {
			continue
		}
		var rs []struct {
			Priority int    `json:"priority"`
			Src      string `json:"src"`
			Srclen   *int   `json:"srclen"`
			Dst      string `json:"dst"`
			Dstlen   *int   `json:"dstlen"`
			Table    string `json:"table"`
			Suppress *int   `json:"suppress_prefixlen"`
			IIF      string `json:"iif"`
			OIF      string `json:"oif"`
		}
		if json.Unmarshal([]byte(s), &rs) != nil {
			continue
		}
		for _, r := range rs {
			if (r.Priority == 0 && r.Table == "local") || (r.Priority == 32766 && r.Table == "main") || (r.Priority == 32767 && r.Table == "default") {
				continue
			}
			pfx := func(a string, l *int) string {
				if a == "" || a == "all" {
					return ""
				}
				if l != nil {
					return a + "/" + strconv.Itoa(*l)
				}
				return a
			}
			out = append(out, NetRule{Family: fam, Priority: r.Priority, From: pfx(r.Src, r.Srclen), To: pfx(r.Dst, r.Dstlen), Table: r.Table, Suppress: r.Suppress, IIF: r.IIF, OIF: r.OIF})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

func (st *netState) link(name string) *NetLink {
	for i := range st.links {
		if st.links[i].Name == name {
			return &st.links[i]
		}
	}
	return nil
}

// critical works out which interfaces the browser's connection to Homestack
// depends on: the one with the address it connected to, plus everything
// below it (a bridge's ports, a bond's ports, a VLAN's parent). If that
// can't be told (a VPN, a proxy, an unknown address), every interface with
// a default route counts, to be safe.
func (st *netState) critical(via string) (yours string, set map[string]bool) {
	set = map[string]bool{}
	ip, err := netip.ParseAddr(via)
	var roots []string
	if err == nil {
		for _, l := range st.links {
			for _, a := range l.Addrs {
				if p, err := netip.ParsePrefix(a.Addr); err == nil && p.Addr() == ip.Unmap() {
					yours = l.Name
				}
			}
		}
	}
	if l := st.link(yours); yours != "" && l != nil && l.Type != "wireguard" && l.Type != "tunnel" && l.Type != "loopback" && l.Type != "other" {
		roots = []string{yours}
	} else {
		for _, r := range st.routes {
			if r.Table == "main" && r.Dst == "default" && r.Dev != "" && !slices.Contains(roots, r.Dev) {
				roots = append(roots, r.Dev)
			}
		}
	}
	var walk func(name string)
	walk = func(name string) {
		if set[name] {
			return
		}
		set[name] = true
		for _, l := range st.links {
			if l.Master == name {
				walk(l.Name)
			}
		}
		if l := st.link(name); l != nil && l.Parent != "" {
			walk(l.Parent)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return yours, set
}
