package agent

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// A real server's services link, with its policy routing.
func policyBr0(t *testing.T) string {
	b, err := os.ReadFile("testdata/net/policy-br0.network")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func keep(addrs ...string) []AddrEdit {
	var out []AddrEdit
	for _, a := range addrs {
		out = append(out, AddrEdit{Was: a, Now: a})
	}
	return out
}

func editOf(c LinkConfig) LinkConfigEdit {
	return LinkConfigEdit{DHCP: c.DHCP, Addresses: keep(c.Addresses...), Gateway4: c.Gateway4, Gateway6: c.Gateway6, DNS: c.DNS, MTU: c.MTU}
}

func TestReadLinkConfigLucy(t *testing.T) {
	c := readLinkConfig("20-br0.network", policyBr0(t))
	if !slices.Equal(c.Addresses, []string{"10.0.10.5/24", "2001:db8:0:8a00::5/64"}) {
		t.Errorf("addresses %v", c.Addresses)
	}
	if c.Gateway4 != "10.0.10.1" || c.Gateway6 != "fe80::1" {
		t.Errorf("gateways %q %q", c.Gateway4, c.Gateway6)
	}
	if len(c.DNS) != 6 || c.DHCP != "no" {
		t.Errorf("dns %v dhcp %q", c.DNS, c.DHCP)
	}
	if c.Policy != 4+6 { // four table-100 routes, six rules
		t.Errorf("policy %d", c.Policy)
	}
}

func TestApplyLinkConfigUnchangedIsIdentical(t *testing.T) {
	text := policyBr0(t)
	out, _, err := applyLinkConfig(text, editOf(readLinkConfig("", text)))
	if err != nil || out != text {
		t.Fatalf("an unchanged form must leave the file byte for byte: err %v\n%s", err, out)
	}
}

func TestApplyLinkConfigGatewayFollowsPolicyRoutes(t *testing.T) {
	text := policyBr0(t)
	e := editOf(readLinkConfig("", text))
	e.Gateway4 = "10.0.10.254"
	out, notes, err := applyLinkConfig(text, e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Gateway=10.0.10.1\n") || strings.Count(out, "Gateway=10.0.10.254") != 2 {
		t.Errorf("both the main and table 100 routes should move:\n%s", out)
	}
	if len(notes) == 0 {
		t.Error("expected a note about the policy routes")
	}
	// Everything else is untouched: same lines apart from the two gateways.
	a, b := strings.Split(text, "\n"), strings.Split(out, "\n")
	diff := 0
	for i := range a {
		if a[i] != b[i] {
			diff++
		}
	}
	if len(a) != len(b) || diff != 2 {
		t.Errorf("expected exactly 2 changed lines, got %d (lines %d vs %d)", diff, len(a), len(b))
	}
}

func TestApplyLinkConfigAddresses(t *testing.T) {
	text := policyBr0(t)
	e := editOf(readLinkConfig("", text))
	// Rename the IPv4 address in place, drop the IPv6 one, add another.
	e.Addresses = []AddrEdit{{Was: "10.0.10.5/24", Now: "10.0.10.9/24"}, {Now: "10.0.10.15/24"}}
	e.Gateway6 = "" // the IPv6 network goes away with its address
	out, _, err := applyLinkConfig(text, e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[Address]\nAddress=10.0.10.9/24\nRouteMetric=100") {
		t.Errorf("the [Address] section should keep its RouteMetric:\n%s", out)
	}
	if strings.Contains(out, "::5/64") {
		t.Error("the IPv6 address should be gone")
	}
	if !strings.Contains(out, "Address=10.0.10.15/24") {
		t.Error("the new address is missing")
	}
	if got := readLinkConfig("", out).Addresses; !slices.Contains(got, "10.0.10.15/24") || len(got) != 2 {
		t.Errorf("read back %v", got)
	}
}

func TestApplyLinkConfigSwapAddresses(t *testing.T) {
	text := "[Match]\nName=x\n\n[Network]\nAddress=10.0.0.2/24\nAddress=10.0.0.3/24\n"
	e := LinkConfigEdit{DHCP: "no", Addresses: []AddrEdit{{"10.0.0.2/24", "10.0.0.3/24"}, {"10.0.0.3/24", "10.0.0.2/24"}}, DNS: []string{}}
	out, _, err := applyLinkConfig(text, e)
	if err != nil {
		t.Fatal(err)
	}
	if out != "[Match]\nName=x\n\n[Network]\nAddress=10.0.0.3/24\nAddress=10.0.0.2/24\n" {
		t.Errorf("swap:\n%s", out)
	}
}

func TestApplyLinkConfigDNSAndMTU(t *testing.T) {
	text := policyBr0(t)
	e := editOf(readLinkConfig("", text))
	e.DNS = []string{"9.9.9.9", "2620:fe::fe"}
	e.MTU = 9000
	out, notes, err := applyLinkConfig(text, e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "IPv6PrivacyExtensions=no\nDNS=9.9.9.9\nDNS=2620:fe::fe\nConfigureWithoutCarrier=yes") {
		t.Errorf("DNS should be replaced where it was:\n%s", out)
	}
	if !strings.HasSuffix(out, "[Link]\nMTUBytes=9000\n") || len(notes) == 0 {
		t.Errorf("MTU:\n%s", out)
	}
}

func TestApplyLinkConfigRefusals(t *testing.T) {
	text := policyBr0(t)
	base := editOf(readLinkConfig("", text))
	for name, mod := range map[string]func(e *LinkConfigEdit){
		"gateway outside":   func(e *LinkConfigEdit) { e.Gateway4 = "192.168.1.1" },
		"network address":   func(e *LinkConfigEdit) { e.Addresses = []AddrEdit{{Now: "10.0.10.0/24"}} },
		"broadcast":         func(e *LinkConfigEdit) { e.Addresses = []AddrEdit{{Now: "10.0.10.255/24"}} },
		"not an address":    func(e *LinkConfigEdit) { e.Addresses = []AddrEdit{{Now: "10.0.10.5"}} },
		"bad dns":           func(e *LinkConfigEdit) { e.DNS = []string{"one.one.one.one"} },
		"mtu":               func(e *LinkConfigEdit) { e.MTU = 100 },
		"duplicate":         func(e *LinkConfigEdit) { e.Addresses = append(e.Addresses, AddrEdit{Now: "10.0.10.5/24"}) },
		"huge network":      func(e *LinkConfigEdit) { e.Addresses = []AddrEdit{{Now: "10.0.0.5/4"}} },
		"overlapping masks": func(e *LinkConfigEdit) { e.Addresses = append(e.Addresses, AddrEdit{Now: "10.0.10.7/16"}) },
	} {
		e := base
		e.Addresses = slices.Clone(base.Addresses)
		mod(&e)
		if _, _, err := applyLinkConfig(text, e); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	port := "[Match]\nName=bond0\n\n[Network]\nBridge=br0\n"
	if _, _, err := applyLinkConfig(port, LinkConfigEdit{Addresses: []AddrEdit{{Now: "10.0.10.8/24"}}}); err == nil {
		t.Error("a bridge port must not get an address")
	}
}

func TestValidateNetFile(t *testing.T) {
	good := map[string]string{
		"20-br0.network":    "[Match]\nName=br0\n\n[Network]\nAddress=10.0.10.5/24\n",
		"10-bond0.netdev":   "[NetDev]\nName=bond0\nKind=bond\n",
		"10-mac.network":    "# comment\n[Match]\nPermanentMACAddress=1c:86:0b:2c:ee:02\n[Network]\nDHCP=yes\n",
		"99-continued.link": "[Match]\nName=en*\n[Link]\nNamePolicy=kernel \\\n  database\n",
	}
	for name, text := range good {
		if err := validateNetFile(name, text); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"all.network":         "[Network]\nDHCP=yes\n",            // no [Match]: every interface
		"empty-match.network": "[Match]\n\n[Network]\nDHCP=yes\n", // [Match] with nothing in it
		"x.netdev":            "[NetDev]\nName=br9\n",             // no Kind
		"y.netdev":            "[NetDev]\nName=this-name-is-too-long\nKind=bridge\n",
		"z.network":           "Name=br0\n[Match]\nName=br0\n", // key before any section
		"w.network":           "[Match]\nName=br0\njunk line\n",
		"../escape.network":   "[Match]\nName=br0\n",
		"v.conf":              "[Match]\nName=br0\n",
	}
	for name, text := range bad {
		if err := validateNetFile(name, text); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
