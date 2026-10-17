package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// netRunner answers the network commands from output captured on Debian 13
// (testdata/net: a lucy-style bond0 → br0, VLAN 40 → br0.40, and a separate
// management port mgmt0). It's safe for the concurrent gateway pings.
type netRunner struct {
	mu   sync.Mutex
	dir  string // the test's /etc/systemd/network
	out  map[string]string
	ran  []string
	ping bool // do gateways answer?
}

func newNetRunner(t *testing.T) (*netRunner, string) {
	t.Helper()
	dir := t.TempDir()
	src := "testdata/net/etc"
	des, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		b, _ := os.ReadFile(filepath.Join(src, de.Name()))
		if err := os.WriteFile(filepath.Join(dir, de.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join("testdata/net", f))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "/etc/systemd/network", filepath.ToSlash(dir))
	}
	r := &netRunner{dir: dir, out: map[string]string{
		"systemctl is-active systemd-networkd": "active\n",
		"ip -j -d addr show":                   read("ip-addr.json"),
		"networkctl --json=short list":         read("networkctl.json"),
		"resolvectl dns":                       read("resolvectl.txt"),
		"ip -4 -j route show table all":        read("route4.json"),
		"ip -6 -j route show table all":        read("route6.json"),
		"ip -4 -j rule show":                   read("rule4.json"),
		"ip -6 -j rule show":                   read("rule6.json"),
		"networkctl reload":                    "",
		"networkctl delete":                    "",
		"networkctl up":                        "",
		"networkctl down":                      "",
		"journalctl":                           "",
	}}
	return r, dir
}

func (r *netRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, line)
	if name == "ping" {
		if r.ping {
			return nil, nil
		}
		return nil, errors.New("exit 1")
	}
	best := ""
	for k := range r.out {
		if strings.HasPrefix(line, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return nil, errors.New("exit 1: " + line)
	}
	return []byte(r.out[best]), nil
}

func (r *netRunner) ran1(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.ran, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

func testNet(t *testing.T) (*netManager, *netRunner, string) {
	r, dir := newNetRunner(t)
	a := New(Options{Runner: r, StateDir: t.TempDir(), NetDir: dir})
	a.net.settle, a.net.checkFor = 5*time.Millisecond, 50*time.Millisecond
	return a.net, r, dir
}

func TestNetStateLucyLayout(t *testing.T) {
	n, _, _ := testNet(t)
	st := n.readNetState(context.Background())
	if !st.managed {
		t.Fatal("networkd should count as running")
	}
	br0, bond, vlan, port := st.link("br0"), st.link("bond0"), st.link("bond0.40"), st.link("port0")
	if br0 == nil || bond == nil || vlan == nil || port == nil {
		t.Fatalf("missing links: %+v", st.links)
	}
	if br0.Type != "bridge" || bond.Type != "bond" || vlan.Type != "vlan" || vlan.VLAN != 40 || vlan.Parent != "bond0" || vlan.Master != "br0.40" {
		t.Errorf("types: br0 %s bond0 %s vlan %+v", br0.Type, bond.Type, vlan)
	}
	if bond.BondMode != "active-backup" || bond.Active == "" || bond.Master != "br0" || port.Master != "bond0" || port.PortState == "" {
		t.Errorf("bond: %+v port: %+v", bond, port)
	}
	if br0.NetworkFile != "20-br0.network" || br0.NetDevFile != "20-br0.netdev" || br0.Config == nil || br0.Config.Gateway4 != "10.0.10.1" {
		t.Errorf("br0 files/config: %q %q %+v", br0.NetworkFile, br0.NetDevFile, br0.Config)
	}
	if !slices.Contains(br0.DNS, "1.1.1.2") {
		t.Errorf("br0 DNS %v", br0.DNS)
	}
	if len(st.rules) != 1 || st.rules[0].Table != "100" || st.rules[0].From != "10.0.10.5" {
		t.Errorf("rules %+v", st.rules)
	}
	if l := st.link("eth0"); l == nil || l.Type != "veth" {
		t.Errorf("container's own eth0: %+v", l)
	}
}

func TestCriticalLinks(t *testing.T) {
	n, _, _ := testNet(t)
	st := n.readNetState(context.Background())
	yours, set := st.critical("10.0.10.6")
	if yours != "mgmt0" || !set["mgmt0"] || set["br0"] || set["bond0"] {
		t.Errorf("via the management port: %q %v", yours, set)
	}
	yours, set = st.critical("10.0.10.5")
	if yours != "br0" || !set["br0"] || !set["bond0"] || !set["port0"] || !set["port1"] || set["br0.40"] {
		t.Errorf("via br0, everything under it is critical: %q %v", yours, set)
	}
	// An address we can't place (a VPN, a proxy): every link with a
	// default route counts.
	_, set = st.critical("192.168.99.1")
	if !set["br0"] || !set["mgmt0"] {
		t.Errorf("unknown: %v", set)
	}
}

func TestPlanVLANAdd(t *testing.T) {
	n, _, _ := testNet(t)
	ch, st, err := n.plan(context.Background(), NetOp{Op: "vlan-add", Parent: "bond0", VLAN: 50, Via: "10.0.10.6"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"30-bond0.50.netdev", "30-bond0.50.network", "30-br0.50.netdev", "30-br0.50.network", "20-bond0.network"} {
		if ch.files[f] == nil {
			t.Errorf("missing %s in %v", f, ch.files)
		}
	}
	if got := *ch.files["20-bond0.network"]; got != st.files["20-bond0.network"]+"VLAN=bond0.50\n" {
		t.Errorf("parent file:\n%s", got)
	}
	if ch.critical != "" {
		t.Errorf("connected through mgmt0, so bond0 isn't critical: %q", ch.critical)
	}
	// Through br0 the parent is critical: the change needs typing its name.
	ch, _, _ = n.plan(context.Background(), NetOp{Op: "vlan-add", Parent: "bond0", VLAN: 50, Via: "10.0.10.5"})
	if ch.critical != "bond0" {
		t.Errorf("critical %q", ch.critical)
	}
	for name, op := range map[string]NetOp{
		"duplicate":   {Op: "vlan-add", Parent: "bond0", VLAN: 40},
		"bad id":      {Op: "vlan-add", Parent: "bond0", VLAN: 4095},
		"on a bridge": {Op: "vlan-add", Parent: "br0", VLAN: 60},
		"taken name":  {Op: "vlan-add", Parent: "bond0", VLAN: 60, Bridge: "br0"},
		"overlap":     {Op: "vlan-add", Parent: "bond0", VLAN: 60, Address: "10.0.10.50/24"},
	} {
		if _, _, err := n.plan(context.Background(), op); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestPlanVLANRemove(t *testing.T) {
	n, _, _ := testNet(t)
	ch, st, err := n.plan(context.Background(), NetOp{Op: "vlan-remove", Bridge: "br0.40", Via: "10.0.10.6"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"30-bond0.40.netdev", "30-bond0.40.network", "30-br0.40.netdev", "30-br0.40.network"} {
		if v, ok := ch.files[f]; !ok || v != nil {
			t.Errorf("%s should be deleted", f)
		}
	}
	if strings.Contains(*ch.files["20-bond0.network"], "VLAN=bond0.40") {
		t.Error("the parent still has VLAN=bond0.40")
	}
	p := n.planView(ch, st)
	if !slices.Equal(p.Remove, []string{"bond0.40", "br0.40"}) {
		t.Errorf("interfaces to delete: %v", p.Remove)
	}
	if _, _, err := n.plan(context.Background(), NetOp{Op: "vlan-remove", Bridge: "br0"}); err == nil {
		t.Error("br0 isn't a VLAN bridge")
	}
}

func TestPlanGuards(t *testing.T) {
	n, _, _ := testNet(t)
	ctx := context.Background()
	down, up := false, true
	refuse := map[string]NetOp{
		"down your link":      {Op: "link-state", Link: "mgmt0", Up: &down, Via: "10.0.10.6"},
		"down below you":      {Op: "link-state", Link: "port0", Up: &down, Via: "10.0.10.5"},
		"up already":          {Op: "link-state", Link: "br0", Up: &up},
		"docker's":            {Op: "link-state", Link: "eth0", Up: &down},
		"remove your address": {Op: "link-config", Link: "mgmt0", Via: "10.0.10.6", Config: &LinkConfigEdit{DNS: []string{"1.1.1.2"}, Addresses: []AddrEdit{{Was: "10.0.10.6/24", Now: "10.0.10.7/24"}}, Gateway4: "10.0.10.1"}},
		"dhcp on your link":   {Op: "link-config", Link: "mgmt0", Via: "10.0.10.6", Config: &LinkConfigEdit{DHCP: "yes", DNS: []string{"1.1.1.2"}, Addresses: keep("10.0.10.6/24"), Gateway4: "10.0.10.1"}},
		"match-all file":      {Op: "files", Files: map[string]*string{"50-all.network": new("[Network]\nDHCP=yes\n")}},
		"duplicate netdev":    {Op: "files", Files: map[string]*string{"50-x.netdev": new("[NetDev]\nName=br0\nKind=bridge\n")}},
		"delete everything":   {Op: "files", Files: map[string]*string{"10-bond0-ports.network": nil, "15-mgmt.network": nil, "20-bond0.network": nil, "20-br0.network": nil, "30-bond0.40.network": nil, "30-br0.40.network": nil}},
		"no change":           {Op: "files", Files: map[string]*string{"20-br0.netdev": new("[NetDev]\nName=br0\nKind=bridge\n\n[Bridge]\nSTP=no\nForwardDelaySec=0\n")}},
	}
	for name, op := range refuse {
		if _, _, err := n.plan(ctx, op); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	// Taking down a link your connection doesn't use is fine.
	if ch, _, err := n.plan(ctx, NetOp{Op: "link-state", Link: "port0", Up: &down, Via: "10.0.10.6"}); err != nil || ch.linkOps["port0"] {
		t.Errorf("port0 down via mgmt0: %v %v", err, ch.linkOps)
	}
	// Editing the file that sets up the management port is critical.
	ch, _, err := n.plan(ctx, NetOp{Op: "files", Via: "10.0.10.6", Files: map[string]*string{"15-mgmt.network": new("[Match]\nName=mgmt0\n\n[Network]\nDHCP=yes\n")}})
	if err != nil || ch.critical != "mgmt0" {
		t.Errorf("editing mgmt0's file: %v %q", err, ch.critical)
	}
}

func TestApplyKeepAndUndo(t *testing.T) {
	n, r, dir := testNet(t)
	ctx := context.Background()
	op := NetOp{Op: "vlan-add", Parent: "bond0", VLAN: 50, Via: "10.0.10.6"}
	p, err := n.apply(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "30-br0.50.network")); err != nil {
		t.Fatal("the files should be written")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "30-br0.50.netdev")); fi.Mode().Perm() != 0o644 && filepath.Separator == '/' {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if !r.ran1("networkctl reload") {
		t.Error("networkd wasn't reloaded")
	}
	if _, err := n.apply(ctx, op); err == nil {
		t.Error("a second change must wait for the first to be kept or undone")
	}
	if _, err := os.Stat(n.pendingPath()); err != nil {
		t.Error("the pending change must be saved, so a restart undoes it")
	}
	if err := n.cancel(ctx, p.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "30-br0.50.network")); !errors.Is(err, os.ErrNotExist) {
		t.Error("undo should remove the new files")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "20-bond0.network"))
	if strings.Contains(string(b), "bond0.50") {
		t.Error("undo should restore the parent file")
	}
	if n.view(ctx, "").Last == nil {
		t.Error("the undo should be reported")
	}
	// Apply again and keep it.
	p, err = n.apply(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.confirm("wrong"); err == nil {
		t.Error("a wrong token must not keep it")
	}
	if err := n.confirm(p.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "30-br0.50.network")); err != nil {
		t.Error("a kept change stays")
	}
	bs := n.backups()
	if len(bs) < 3 || !slices.ContainsFunc(bs, func(b NetBackup) bool { return b.ID == "original" }) {
		t.Errorf("backups %+v", bs)
	}
}

func TestApplyNeedsTypedNameForYourConnection(t *testing.T) {
	n, _, _ := testNet(t)
	op := NetOp{Op: "vlan-add", Parent: "bond0", VLAN: 50, Via: "10.0.10.5"}
	if _, err := n.apply(context.Background(), op); err == nil {
		t.Fatal("expected a request to type bond0")
	}
	op.ConfirmText = "bond0"
	if _, err := n.apply(context.Background(), op); err != nil {
		t.Fatal(err)
	}
}

func TestApplyUndoesWhenGatewayLost(t *testing.T) {
	n, r, dir := testNet(t)
	r.ping = true
	before, _ := os.ReadFile(filepath.Join(dir, "15-mgmt.network"))
	// After writing, gateways stop answering: the change must be undone at once.
	n.a.run = &failAfterReload{netRunner: r}
	_, err := n.apply(context.Background(), NetOp{Op: "files", Via: "10.0.10.6", Files: map[string]*string{"15-mgmt.network": new(strings.Replace(string(before), "DNS=1.1.1.2", "DNS=9.9.9.9", 1))}, ConfirmText: "mgmt0"})
	if err == nil || !strings.Contains(err.Error(), "undone") {
		t.Fatalf("expected an automatic undo, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "15-mgmt.network"))
	if string(after) != string(before) {
		t.Error("the file should be back as it was")
	}
	if _, err := os.Stat(n.pendingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("nothing should be left pending")
	}
}

// failAfterReload makes pings fail once networkd has reloaded.
type failAfterReload struct {
	*netRunner
	reloaded bool
}

func (f *failAfterReload) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "networkctl" && len(args) > 0 && args[0] == "reload" {
		f.mu.Lock()
		f.reloaded = !f.reloaded // the second reload is the undo
		f.ping = !f.reloaded
		f.mu.Unlock()
	}
	return f.netRunner.Run(ctx, name, args...)
}

func TestRecoverUndoesUnkeptChange(t *testing.T) {
	n, r, dir := testNet(t)
	if _, err := n.apply(context.Background(), NetOp{Op: "vlan-add", Parent: "bond0", VLAN: 50, Via: "10.0.10.6"}); err != nil {
		t.Fatal(err)
	}
	n.timer.Stop()
	// A fresh agent (after a restart) finds the change nobody kept.
	a2 := New(Options{Runner: r, StateDir: n.a.opt.StateDir, NetDir: dir})
	a2.net.recover(context.Background(), "restarted")
	if _, err := os.Stat(filepath.Join(dir, "30-br0.50.network")); !errors.Is(err, os.ErrNotExist) {
		t.Error("recover should undo the change")
	}
	// And a later agent can still say what happened.
	a3 := New(Options{Runner: r, StateDir: n.a.opt.StateDir, NetDir: dir})
	if l := a3.net.view(context.Background(), "").Last; l == nil || !strings.Contains(l.Text, "restarted") {
		t.Errorf("the undo should be remembered across restarts: %+v", l)
	}
}
