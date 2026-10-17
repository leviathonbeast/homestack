package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"homestack/internal/agent"
	"homestack/internal/docker"
	"homestack/internal/libvirt"
)

// The Networks page's Host and VMs tabs, and the Docker tab's LAN networks.
//
// Changes to the host's own network are made by the host agent, which does
// them safely (internal/agent/network.go: a review, refusals, a gateway
// test, and an undo unless kept). Homestack adds what only it knows: the
// address the browser really connected to, and which VMs and Docker
// networks use each interface.

// agentSlow is for network changes, which wait for interfaces to settle and
// gateways to answer before they return.
var agentSlow = &http.Client{Timeout: 90 * time.Second, Transport: agentClient.Transport}

func (s *Server) agentDo(ctx context.Context, c *http.Client, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, "http://agent"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return resp.StatusCode, errors.New(e.Error)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

func (s *Server) needAgent(w http.ResponseWriter) bool {
	if !s.agentAvailable() {
		writeErr(w, http.StatusServiceUnavailable, "The host service isn't running, so the server's own network can't be shown or changed. Install it with deploy/debian/agent/install.sh.")
		return false
	}
	return true
}

// linkUsage is what depends on a host interface.
type linkUsage struct {
	VMs    []string `json:"vms"`    // VMs with a network card on this bridge
	Docker []string `json:"docker"` // Docker LAN networks with this as their parent
}

// netUsage maps interfaces to the VMs and Docker networks on them. VM
// definitions are read with virsh, so the answer is cached briefly.
type netUsageCache struct {
	mu  sync.Mutex
	at  time.Time
	vms []libvirt.VM
}

func (s *Server) vmsCached(ctx context.Context) []libvirt.VM {
	c := &s.netUsage
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > 10*time.Second {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		vms, err := s.vms.List(cctx)
		if err != nil {
			vms = nil
		}
		c.vms, c.at = vms, time.Now()
	}
	return c.vms
}

func (s *Server) linkUsages(ctx context.Context) map[string]*linkUsage {
	out := map[string]*linkUsage{}
	get := func(name string) *linkUsage {
		if out[name] == nil {
			out[name] = &linkUsage{VMs: []string{}, Docker: []string{}}
		}
		return out[name]
	}
	for _, vm := range s.vmsCached(ctx) {
		for _, n := range vm.NICs {
			if n.Kind == "bridge" && n.Source != "" && !slices.Contains(get(n.Source).VMs, vm.Name) {
				get(n.Source).VMs = append(get(n.Source).VMs, vm.Name)
			}
		}
	}
	if nets, err := s.docker.Networks(ctx); err == nil {
		for _, n := range nets {
			if p := n.Options["parent"]; p != "" {
				get(p).Docker = append(get(p).Docker, n.Name)
			}
		}
	}
	return out
}

type hostNetView struct {
	agent.NetView
	Rates map[string][2]float64 `json:"rates"` // bytes/s received, sent
	Usage map[string]*linkUsage `json:"usage"`
	Via   string                `json:"via"` // the address this browser reached Homestack on
}

func (s *Server) netHost(w http.ResponseWriter, r *http.Request) {
	if !s.needAgent(w) {
		return
	}
	via := localIP(r)
	var v hostNetView
	if _, err := s.agentDo(r.Context(), agentClient, "GET", "/v1/net?via="+via, nil, &v.NetView); err != nil {
		writeErr(w, http.StatusBadGateway, "Couldn't read the network from the host service: "+err.Error())
		return
	}
	v.Rates = s.metrics.host.Rates()
	if v.Rates == nil {
		v.Rates = map[string][2]float64{}
	}
	v.Usage = s.linkUsages(r.Context())
	v.Via = via
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) netHostFile(w http.ResponseWriter, r *http.Request) {
	if !s.needAgent(w) {
		return
	}
	s.proxyAgent("GET", "/v1/net/file")(w, r)
}

// netHostChange passes a planned or applied change to the agent, adding the
// address the browser really used (never trusting the page for that) and
// refusing to remove bridges that VMs or Docker networks depend on.
func (s *Server) netHostChange(apply bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.needAgent(w) {
			return
		}
		var op map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&op); err != nil {
			writeErr(w, http.StatusBadRequest, "Couldn't read the request.")
			return
		}
		op["via"] = localIP(r)
		kind, _ := op["op"].(string)
		usage := s.linkUsages(r.Context())
		var notes []string
		switch kind {
		case "vlan-remove":
			b, _ := op["bridge"].(string)
			if u := usage[b]; u != nil && len(u.VMs) > 0 {
				writeErr(w, http.StatusConflict, fmt.Sprintf("%s is used by %s. Move %s to another network first (VM → Settings → Network).", b, strings.Join(u.VMs, ", "), map[bool]string{true: "them", false: "it"}[len(u.VMs) > 1]))
				return
			}
			if u := usage[b]; u != nil && len(u.Docker) > 0 {
				writeErr(w, http.StatusConflict, fmt.Sprintf("The Docker network %s uses %s. Remove it first (Docker tab).", strings.Join(u.Docker, ", "), b))
				return
			}
		case "link-state":
			l, _ := op["link"].(string)
			if up, _ := op["up"].(bool); !up {
				if u := usage[l]; u != nil {
					if len(u.VMs) > 0 {
						notes = append(notes, "VMs on "+l+" lose their network while it's down: "+strings.Join(u.VMs, ", ")+".")
					}
					if len(u.Docker) > 0 {
						notes = append(notes, "Containers on the Docker network "+strings.Join(u.Docker, ", ")+" lose their network while it's down.")
					}
				}
			}
		}
		if !apply {
			var plan map[string]any
			if code, err := s.agentDo(r.Context(), agentClient, "POST", "/v1/net/plan", op, &plan); err != nil {
				writeErr(w, statusOr(code), err.Error())
				return
			}
			if len(notes) > 0 {
				have, _ := plan["notes"].([]any)
				for _, n := range notes {
					have = append(have, n)
				}
				plan["notes"] = have
			}
			writeJSON(w, http.StatusOK, plan)
			return
		}
		var res map[string]any
		if code, err := s.agentDo(context.WithoutCancel(r.Context()), agentSlow, "POST", "/v1/net/apply", op, &res); err != nil {
			writeErr(w, statusOr(code), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// statusOr passes on the agent's status, or 502 when it couldn't be reached.
func statusOr(code int) int {
	if code == 0 {
		return http.StatusBadGateway
	}
	return code
}

func (s *Server) netHostConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.needAgent(w) {
		return
	}
	s.proxyAgent("POST", "/v1/net/confirm")(w, r)
}

func (s *Server) netHostCancel(w http.ResponseWriter, r *http.Request) {
	if !s.needAgent(w) {
		return
	}
	var req map[string]any
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	if code, err := s.agentDo(context.WithoutCancel(r.Context()), agentSlow, "POST", "/v1/net/cancel", req, nil); err != nil {
		writeErr(w, statusOr(code), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- VM networks (libvirt) ----------

type vmNetView struct {
	libvirt.VNetwork
	VMs []vmNetMember `json:"vms"`
}

type vmNetMember struct {
	Name  string `json:"name"`
	State string `json:"state"`
	MAC   string `json:"mac"`
}

func (s *Server) netVMs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	nets, err := s.vms.NetworkList(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "error": sentence(err.Error())})
		return
	}
	vms, _ := s.vms.List(ctx)
	s.netUsage.mu.Lock()
	s.netUsage.vms, s.netUsage.at = vms, time.Now()
	s.netUsage.mu.Unlock()
	byNet := map[string][]vmNetMember{}
	byBridge := map[string][]vmNetMember{}
	for _, vm := range vms {
		for _, n := range vm.NICs {
			m := vmNetMember{vm.Name, vm.State, n.MAC}
			switch n.Kind {
			case "network":
				byNet[n.Source] = append(byNet[n.Source], m)
			case "bridge":
				byBridge[n.Source] = append(byBridge[n.Source], m)
			}
		}
	}
	out := []vmNetView{}
	for _, n := range nets {
		v := vmNetView{VNetwork: n, VMs: byNet[n.Name]}
		if v.VMs == nil {
			v.VMs = []vmNetMember{}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	type bridgeView struct {
		Name string        `json:"name"`
		VMs  []vmNetMember `json:"vms"`
	}
	bridges := []bridgeView{}
	for _, b := range hostBridges() {
		v := bridgeView{Name: b, VMs: byBridge[b]}
		if v.VMs == nil {
			v.VMs = []vmNetMember{}
		}
		bridges = append(bridges, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "networks": out, "bridges": bridges, "suggest": freeSubnet(s.usedSubnets(ctx))})
}

// freeSubnet suggests a 192.168.x.0/24 nothing on this server uses yet.
func freeSubnet(used map[string]netip.Prefix) string {
	for i := 100; i < 255; i++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(i), 0}), 24)
		free := true
		for _, u := range used {
			if u.Overlaps(p) {
				free = false
			}
		}
		if free {
			return p.String()
		}
	}
	return ""
}

// usedSubnets are networks already in use on this server: its own
// interfaces, Docker's networks and libvirt's.
func (s *Server) usedSubnets(ctx context.Context) map[string]netip.Prefix {
	out := map[string]netip.Prefix{}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() && !p.Addr().IsLoopback() {
				out[i.Name+" ("+p.Masked().String()+")"] = p.Masked()
			}
		}
	}
	if nets, err := s.docker.Networks(ctx); err == nil {
		for _, n := range nets {
			for _, c := range n.IPAM.Config {
				if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
					out["the Docker network "+n.Name] = p.Masked()
				}
			}
		}
	}
	if nets, err := s.vms.NetworkList(ctx); err == nil {
		for _, n := range nets {
			if p, err := netip.ParsePrefix(n.Address); err == nil {
				out["the VM network "+n.Name] = p.Masked()
			}
		}
	}
	return out
}

func (s *Server) createVMNet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Mode      string `json:"mode"`
		Subnet    string `json:"subnet"`
		DHCP      bool   `json:"dhcp"`
		Autostart bool   `json:"autostart"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req.Name = strings.TrimSpace(req.Name)
	if !libvirt.ValidNetworkName(req.Name) {
		writeErr(w, http.StatusBadRequest, "Use letters, digits and . _ - for the name (up to 32).")
		return
	}
	if req.Mode != "nat" && req.Mode != "isolated" {
		writeErr(w, http.StatusBadRequest, "Choose NAT or isolated.")
		return
	}
	p, err := netip.ParsePrefix(strings.TrimSpace(req.Subnet))
	if err != nil || !p.Addr().Is4() {
		writeErr(w, http.StatusBadRequest, "Enter the network as an IPv4 address and size, like 192.168.100.0/24.")
		return
	}
	p = p.Masked()
	if !p.Addr().IsPrivate() {
		writeErr(w, http.StatusBadRequest, "Use a private network (10.x, 172.16–31.x or 192.168.x), so it can't clash with addresses on the internet.")
		return
	}
	if p.Bits() < 16 || p.Bits() > 29 {
		writeErr(w, http.StatusBadRequest, "Use a network between /16 and /29 (/24 gives 253 addresses).")
		return
	}
	used := s.usedSubnets(ctx)
	for what, q := range used {
		if q.Overlaps(p) {
			writeErr(w, http.StatusConflict, fmt.Sprintf("%s overlaps %s. Pick another range, or traffic would go to the wrong place.", p, what))
			return
		}
	}
	if nets, err := s.vms.NetworkList(ctx); err == nil {
		for _, n := range nets {
			if n.Name == req.Name {
				writeErr(w, http.StatusConflict, "There's already a VM network called "+req.Name+".")
				return
			}
		}
	}
	if err := s.vms.NetworkCreate(ctx, req.Name, req.Mode, p, req.DHCP, req.Autostart); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Server) vmNetMembers(ctx context.Context, name string, runningOnly bool) []string {
	vms, _ := s.vms.List(ctx)
	var out []string
	for _, vm := range vms {
		if runningOnly && vm.State != "running" && vm.State != "paused" {
			continue
		}
		for _, n := range vm.NICs {
			if n.Kind == "network" && n.Source == name && !slices.Contains(out, vm.Name) {
				out = append(out, vm.Name)
			}
		}
	}
	return out
}

func (s *Server) vmNetAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Action string `json:"action"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if req.Action == "stop" {
		if on := s.vmNetMembers(ctx, name, true); len(on) > 0 {
			writeErr(w, http.StatusConflict, fmt.Sprintf("%s %s running on %s. Shut %s down first, or %s would lose the network.", strings.Join(on, ", "), map[bool]string{true: "are", false: "is"}[len(on) > 1], name, map[bool]string{true: "them", false: "it"}[len(on) > 1], map[bool]string{true: "they", false: "it"}[len(on) > 1]))
			return
		}
	}
	if err := s.vms.NetworkAction(ctx, name, req.Action); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteVMNet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if used := s.vmNetMembers(ctx, name, false); len(used) > 0 {
		writeErr(w, http.StatusConflict, fmt.Sprintf("%s %s set up to use %s. Move %s to another network first (VM → Settings → Network).", strings.Join(used, ", "), map[bool]string{true: "are", false: "is"}[len(used) > 1], name, map[bool]string{true: "them", false: "it"}[len(used) > 1]))
		return
	}
	if err := s.vms.NetworkDelete(ctx, name); err != nil {
		vmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- Docker LAN networks ----------

// lanParent is a host interface a macvlan or ipvlan network can sit on.
type lanParent struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"` // bridge | vlan | bond | port
	Addrs   []string `json:"addrs"`
	Gateway string   `json:"gateway,omitempty"`
	Subnet  string   `json:"subnet,omitempty"`  // suggested
	IPRange string   `json:"ipRange,omitempty"` // suggested
	Docker  []string `json:"docker"`            // Docker networks already on it
}

var reSkipParent = regexp.MustCompile(`^(lo|docker\d*|docker_gwbridge|br-[0-9a-f]{12}|veth.*|virbr.*|vnet\d+|tap.*|wg.*|tun.*|tailscale.*)$`)

// reDockerOwned are interfaces Docker makes for itself.
var reDockerOwned = regexp.MustCompile(`^(lo|docker\d*|docker_gwbridge|br-[0-9a-f]{12}|veth.*)$`)

func ifaceType(name string) string {
	sys := "/sys/class/net/" + name
	if st, err := os.Stat(sys + "/bridge"); err == nil && st.IsDir() {
		return "bridge"
	}
	if st, err := os.Stat(sys + "/bonding"); err == nil && st.IsDir() {
		return "bond"
	}
	if _, err := os.Stat("/proc/net/vlan/" + name); err == nil {
		return "vlan"
	}
	if _, err := os.Stat(sys + "/device"); err == nil {
		return "port"
	}
	return ""
}

// defaultGateways reads each interface's IPv4 default gateway (main table).
func defaultGateways() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return out
	}
	for i, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		gw := netip.AddrFrom4([4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
		if _, ok := out[f[0]]; !ok {
			out[f[0]] = gw.String()
		}
	}
	return out
}

func (s *Server) lanParents(ctx context.Context) []lanParent {
	gws := defaultGateways()
	usage := map[string][]string{}
	if nets, err := s.docker.Networks(ctx); err == nil {
		for _, n := range nets {
			if p := n.Options["parent"]; p != "" {
				usage[p] = append(usage[p], n.Name)
			}
		}
	}
	out := []lanParent{}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if reSkipParent.MatchString(i.Name) || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		t := ifaceType(i.Name)
		if t == "" {
			continue
		}
		// A port that's part of a bond or bridge isn't a parent: its
		// bond or bridge is.
		if _, err := os.Stat("/sys/class/net/" + i.Name + "/master"); err == nil {
			continue
		}
		p := lanParent{Name: i.Name, Type: t, Addrs: []string{}, Gateway: gws[i.Name], Docker: usage[i.Name]}
		if p.Docker == nil {
			p.Docker = []string{}
		}
		addrs, _ := i.Addrs()
		var own []netip.Addr
		var subnet netip.Prefix
		for _, a := range addrs {
			pf, err := netip.ParsePrefix(a.String())
			if err != nil || !pf.Addr().Is4() {
				continue
			}
			p.Addrs = append(p.Addrs, pf.String())
			own = append(own, pf.Addr())
			if !subnet.IsValid() {
				subnet = pf.Masked()
			}
		}
		if subnet.IsValid() {
			p.Subnet = subnet.String()
			if p.Gateway == "" {
				p.Gateway = subnet.Addr().Next().String()
			}
			if gw, err := netip.ParseAddr(p.Gateway); err == nil {
				p.IPRange = suggestRange(subnet, append(own, gw))
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// suggestRange picks a /27 near the top of the subnet that holds none of
// the given addresses: room for 32 containers, away from where routers'
// DHCP usually starts.
func suggestRange(subnet netip.Prefix, avoid []netip.Addr) string {
	if subnet.Bits() > 27 {
		return ""
	}
	size := uint32(1) << (32 - 27)
	base := subnet.Masked().Addr().As4()
	start := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	total := uint32(1) << (32 - subnet.Bits())
	for off := total - size; ; off -= size {
		v := start + off
		cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), 27)
		if !slices.ContainsFunc(avoid, cand.Contains) {
			return cand.String()
		}
		if off == 0 {
			return ""
		}
	}
}

func (s *Server) networkParents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lanParents(r.Context()))
}

var reDockerNetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// dockerNetworkSpec checks a new Docker network and turns it into a spec.
func (s *Server) dockerNetworkSpec(ctx context.Context, req createNetworkReq) (docker.NetworkSpec, error) {
	spec := docker.NetworkSpec{Name: strings.TrimSpace(req.Name), Driver: req.Driver, Internal: req.Internal}
	if !reDockerNetName.MatchString(spec.Name) {
		return spec, errors.New("Use letters, digits and . _ - for the name.")
	}
	if spec.Driver == "" {
		spec.Driver = "bridge"
	}
	subnetOf := func(field, v string) (netip.Prefix, error) {
		p, err := netip.ParsePrefix(strings.TrimSpace(v))
		if err != nil || !p.Addr().Is4() {
			return p, fmt.Errorf("Enter the %s as an IPv4 network, like 10.0.10.0/24.", field)
		}
		return p.Masked(), nil
	}
	hostAddrs := func() []netip.Prefix {
		var out []netip.Prefix
		ifs, _ := net.Interfaces()
		for _, i := range ifs {
			addrs, _ := i.Addrs()
			for _, a := range addrs {
				if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() && !p.Addr().IsLoopback() && !reDockerOwned.MatchString(i.Name) {
					out = append(out, p)
				}
			}
		}
		return out
	}
	switch spec.Driver {
	case "bridge":
		if strings.TrimSpace(req.Subnet) == "" {
			return spec, nil // Docker picks a free private range
		}
		p, err := subnetOf("subnet", req.Subnet)
		if err != nil {
			return spec, err
		}
		if !p.Addr().IsPrivate() || p.Bits() < 16 || p.Bits() > 29 {
			return spec, errors.New("Use a private network between /16 and /29, like 172.30.0.0/24.")
		}
		for _, h := range hostAddrs() {
			if h.Masked().Overlaps(p) {
				return spec, fmt.Errorf("%s overlaps your LAN (%s), so containers couldn't reach those machines. Leave it empty and Docker picks a free range.", p, h.Masked())
			}
		}
		spec.Subnet = p.String()
		return spec, nil
	case "macvlan", "ipvlan":
	default:
		return spec, errors.New("The driver must be bridge, macvlan or ipvlan.")
	}

	var parent *lanParent
	for _, p := range s.lanParents(ctx) {
		if p.Name == req.Parent {
			parent = &p
		}
	}
	if parent == nil {
		return spec, errors.New("Choose which of the server's interfaces the containers join (br0, a VLAN bridge…).")
	}
	if len(parent.Docker) > 0 {
		return spec, fmt.Errorf("The Docker network %s already uses %s. Containers can join that one; Docker allows one LAN network per interface.", strings.Join(parent.Docker, ", "), parent.Name)
	}
	subnet, err := subnetOf("subnet", req.Subnet)
	if err != nil {
		return spec, err
	}
	gw, err := netip.ParseAddr(strings.TrimSpace(req.Gateway))
	if err != nil || !subnet.Contains(gw) || gw == subnet.Addr() {
		return spec, fmt.Errorf("Enter the router's address on %s as the gateway (usually %s).", subnet, subnet.Addr().Next())
	}
	rng, err := subnetOf("IP range", req.IPRange)
	if err != nil {
		return spec, errors.New("Enter the range of addresses Docker may give containers, like 10.0.10.224/27. Keep it out of your router's DHCP range.")
	}
	if !subnet.Contains(rng.Addr()) || rng.Bits() < subnet.Bits() {
		return spec, fmt.Errorf("The IP range %s must sit inside %s.", rng, subnet)
	}
	if rng.Contains(gw) {
		return spec, fmt.Errorf("The IP range %s includes the gateway %s, so a container could take the router's address.", rng, gw)
	}
	for _, a := range parent.Addrs {
		pa, _ := netip.ParsePrefix(a)
		if pa.Masked() != subnet {
			return spec, fmt.Errorf("%s is on %s, so containers on it need addresses from that network, not %s.", parent.Name, pa.Masked(), subnet)
		}
	}
	for _, h := range hostAddrs() {
		if rng.Contains(h.Addr()) {
			return spec, fmt.Errorf("The IP range %s includes the server's own address %s.", rng, h.Addr())
		}
	}
	spec.Parent, spec.Subnet, spec.Gateway, spec.IPRange = parent.Name, subnet.String(), gw.String(), rng.String()
	return spec, nil
}
