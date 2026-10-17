package libvirt

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

// libvirt's own virtual networks: private networks inside the server that
// VMs can join instead of a host bridge. "nat" gives VMs internet access
// through the server's address; "isolated" connects VMs only to each other
// and the server.

// VNetwork is one libvirt network.
type VNetwork struct {
	Name       string  `json:"name"`
	Active     bool    `json:"active"`
	Autostart  bool    `json:"autostart"`
	Persistent bool    `json:"persistent"`
	Mode       string  `json:"mode"`              // nat | route | open | isolated | bridge | other
	Bridge     string  `json:"bridge,omitempty"`  // virbr0, or the host bridge a "bridge" network uses
	Address    string  `json:"address,omitempty"` // the server's address on it, 192.168.122.1/24
	DHCPStart  string  `json:"dhcpStart,omitempty"`
	DHCPEnd    string  `json:"dhcpEnd,omitempty"`
	Leases     []Lease `json:"leases"`
}

// Lease is an address libvirt's DHCP gave a VM.
type Lease struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
}

type netXML struct {
	Name    string `xml:"name"`
	Forward *struct {
		Mode string `xml:"mode,attr"`
	} `xml:"forward"`
	Bridge struct {
		Name string `xml:"name,attr"`
	} `xml:"bridge"`
	IPs []struct {
		Family  string `xml:"family,attr"`
		Address string `xml:"address,attr"`
		Netmask string `xml:"netmask,attr"`
		Prefix  int    `xml:"prefix,attr"`
		DHCP    *struct {
			Range []struct {
				Start string `xml:"start,attr"`
				End   string `xml:"end,attr"`
			} `xml:"range"`
		} `xml:"dhcp"`
	} `xml:"ip"`
}

func (c *Client) netNames(ctx context.Context, flags ...string) map[string]bool {
	out, err := c.run(ctx, append([]string{"net-list", "--name"}, flags...)...)
	m := map[string]bool{}
	if err != nil {
		return m
	}
	for n := range strings.SplitSeq(out, "\n") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

// NetworkList returns every libvirt network with its settings and leases.
func (c *Client) NetworkList(ctx context.Context) ([]VNetwork, error) {
	if _, err := c.run(ctx, "net-list", "--all", "--name"); err != nil {
		return nil, err
	}
	all := c.netNames(ctx, "--all")
	active := c.netNames(ctx)
	auto := c.netNames(ctx, "--all", "--autostart")
	persistent := c.netNames(ctx, "--all", "--persistent")
	out := []VNetwork{}
	for name := range all {
		n := VNetwork{Name: name, Active: active[name], Autostart: auto[name], Persistent: persistent[name], Leases: []Lease{}}
		raw, err := c.run(ctx, "net-dumpxml", name)
		if err != nil {
			continue
		}
		var x netXML
		if xml.Unmarshal([]byte(raw), &x) != nil {
			continue
		}
		n.Bridge = x.Bridge.Name
		switch {
		case x.Forward == nil:
			n.Mode = "isolated"
		case x.Forward.Mode == "":
			n.Mode = "nat"
		default:
			n.Mode = x.Forward.Mode
		}
		for _, ip := range x.IPs {
			if ip.Family == "ipv6" || n.Address != "" {
				continue
			}
			a, err := netip.ParseAddr(ip.Address)
			if err != nil {
				continue
			}
			bits := ip.Prefix
			if bits == 0 && ip.Netmask != "" {
				if m, err := netip.ParseAddr(ip.Netmask); err == nil {
					for _, b := range m.AsSlice() {
						for ; b != 0; b <<= 1 {
							bits++
						}
					}
				}
			}
			n.Address = netip.PrefixFrom(a, bits).String()
			if ip.DHCP != nil && len(ip.DHCP.Range) > 0 {
				n.DHCPStart, n.DHCPEnd = ip.DHCP.Range[0].Start, ip.DHCP.Range[0].End
			}
		}
		if n.Active && n.DHCPStart != "" {
			n.Leases = c.leases(ctx, name)
		}
		out = append(out, n)
	}
	return out, nil
}

var reLeaseMAC = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

func (c *Client) leases(ctx context.Context, network string) []Lease {
	out, err := c.run(ctx, "net-dhcp-leases", network)
	res := []Lease{}
	if err != nil {
		return res
	}
	// Expiry date, time, MAC, protocol, IP/prefix, hostname, client ID
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !reLeaseMAC.MatchString(f[2]) {
			continue
		}
		l := Lease{MAC: f[2], IP: strings.Split(f[4], "/")[0]}
		if len(f) > 5 && f[5] != "-" {
			l.Hostname = f[5]
		}
		res = append(res, l)
	}
	return res
}

var reNetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

// ValidNetworkName says whether a libvirt network name is acceptable.
func ValidNetworkName(s string) bool { return reNetName.MatchString(s) }

// NetworkCreate defines and starts a NAT or isolated network on subnet,
// with the server at its first address and DHCP handing out the rest.
func (c *Client) NetworkCreate(ctx context.Context, name, mode string, subnet netip.Prefix, dhcp, autostart bool) error {
	if !ValidNetworkName(name) {
		return fmt.Errorf("use letters, digits and . _ - for the name (up to 32)")
	}
	if mode != "nat" && mode != "isolated" {
		return fmt.Errorf("the mode must be nat or isolated")
	}
	subnet = subnet.Masked()
	first := subnet.Addr().Next()
	last := lastAddr(subnet)
	fwd := "\n  <forward mode='nat'/>"
	if mode == "isolated" {
		fwd = ""
	}
	dh := ""
	if dhcp {
		dh = fmt.Sprintf("\n    <dhcp>\n      <range start='%s' end='%s'/>\n    </dhcp>", first.Next(), last.Prev())
	}
	doc := fmt.Sprintf("<network>\n  <name>%s</name>%s\n  <bridge stp='on' delay='0'/>\n  <ip address='%s' prefix='%d'>%s\n  </ip>\n</network>\n", name, fwd, first, subnet.Bits(), dh)
	f, err := os.CreateTemp("", "homestack-net-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(doc); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if _, err := c.run(ctx, "net-define", f.Name()); err != nil {
		return err
	}
	if _, err := c.run(ctx, "net-start", name); err != nil {
		_, _ = c.run(ctx, "net-undefine", name)
		return err
	}
	if autostart {
		if _, err := c.run(ctx, "net-autostart", name); err != nil {
			return err
		}
	}
	return nil
}

// NetworkAction starts, stops or sets a network to start with the server.
func (c *Client) NetworkAction(ctx context.Context, name, action string) error {
	var args []string
	switch action {
	case "start":
		args = []string{"net-start", name}
	case "stop":
		args = []string{"net-destroy", name}
	case "autostart":
		args = []string{"net-autostart", name}
	case "no-autostart":
		args = []string{"net-autostart", "--disable", name}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	_, err := c.run(ctx, args...)
	return err
}

// NetworkDelete stops (if running) and removes a network.
func (c *Client) NetworkDelete(ctx context.Context, name string) error {
	if c.netNames(ctx)[name] {
		if _, err := c.run(ctx, "net-destroy", name); err != nil {
			return err
		}
	}
	if c.netNames(ctx, "--all", "--persistent")[name] {
		_, err := c.run(ctx, "net-undefine", name)
		return err
	}
	return nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := range b {
		for j := 0; j < 8; j++ {
			if i*8+j >= p.Bits() {
				b[i] |= 0x80 >> j
			}
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}
