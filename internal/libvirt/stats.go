package libvirt

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Figures for the VM page: activity counters for every VM from one
// `virsh domstats` call, and what the VM itself reports through the QEMU
// guest agent (its addresses, OS and filesystems).

// DomStats is one VM's counters. They only ever grow while it runs; rates
// come from comparing two readings.
type DomStats struct {
	State   string
	CPUTime uint64 // nanoseconds, all vCPUs
	VCPUs   int
	RSS     uint64 // host memory the VM's process uses, bytes
	// From the guest's balloon driver, when it reports (needs a stats period):
	Avail  uint64 // memory the guest sees, bytes
	Usable uint64 // of that, what programs could still use, bytes
	NetRx  uint64
	NetTx  uint64
	DiskRd uint64
	DiskWr uint64
	Disks  []DiskStat
}

// DiskStat is one disk's size, and the space it takes on the host.
type DiskStat struct {
	Name       string `json:"name"` // vda, sdb...
	Path       string `json:"path"`
	Capacity   uint64 `json:"capacity"`   // size the VM sees
	Allocation uint64 `json:"allocation"` // space used on the host
}

// Stats reads every VM's counters at once.
func (c *Client) Stats(ctx context.Context) (map[string]DomStats, error) {
	raw, _, err := c.domstats(ctx, "--state", "--cpu-total", "--balloon", "--vcpu", "--interface", "--block")
	if err != nil {
		return nil, err
	}
	out := make(map[string]DomStats, len(raw))
	for name, kv := range raw {
		out[name] = parseDomStats(kv)
	}
	return out, nil
}

func parseDomStats(kv map[string]string) DomStats {
	num := func(k string) uint64 {
		v, _ := strconv.ParseUint(kv[k], 10, 64)
		return v
	}
	st := DomStats{
		State:   firstNonEmpty(stateNames[kv["state.state"]], "unknown"),
		CPUTime: num("cpu.time"),
		VCPUs:   int(num("vcpu.current")),
		RSS:     num("balloon.rss") * 1024,
		Avail:   num("balloon.available") * 1024,
		Usable:  num("balloon.usable") * 1024,
	}
	for i := range int(num("net.count")) {
		p := fmt.Sprintf("net.%d.", i)
		st.NetRx += num(p + "rx.bytes")
		st.NetTx += num(p + "tx.bytes")
	}
	for i := range int(num("block.count")) {
		p := fmt.Sprintf("block.%d.", i)
		st.DiskRd += num(p + "rd.bytes")
		st.DiskWr += num(p + "wr.bytes")
		if kv[p+"path"] != "" {
			st.Disks = append(st.Disks, DiskStat{Name: kv[p+"name"], Path: kv[p+"path"], Capacity: num(p + "capacity"), Allocation: num(p + "allocation")})
		}
	}
	return st
}

// IfAddr is one network card and its addresses, as the VM or the host sees it.
type IfAddr struct {
	Name  string   `json:"name"` // inside the VM (from the agent), or the host's vnetN
	MAC   string   `json:"mac"`
	Addrs []string `json:"addrs"` // 192.168.1.50/24; loopback and link-local left out
}

// Addresses asks for a VM's addresses: from the guest agent (every
// interface, the most complete), libvirt's DHCP leases (VMs on a libvirt
// network) or the host's ARP table (VMs on a bridge it has talked to).
func (c *Client) Addresses(ctx context.Context, name, source string) ([]IfAddr, error) {
	out, err := c.run(ctx, "domifaddr", "--source", source, "--domain", name)
	if err != nil {
		return nil, err
	}
	return parseIfAddr(out), nil
}

func parseIfAddr(out string) []IfAddr {
	var list []IfAddr
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || strings.HasPrefix(f[0], "---") || f[0] == "Name" {
			continue
		}
		if f[0] != "-" || len(list) == 0 { // "-" continues the previous card
			list = append(list, IfAddr{Name: f[0], MAC: strings.ToLower(f[1]), Addrs: []string{}})
		}
		addr := strings.TrimSuffix(f[3], "/0") // ARP entries have no prefix length
		ip, err := netip.ParseAddr(strings.Split(addr, "/")[0])
		if err != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		cur := &list[len(list)-1]
		if !slices.Contains(cur.Addrs, addr) {
			cur.Addrs = append(cur.Addrs, addr)
		}
	}
	// Drop cards with nothing to show (lo, or only link-local addresses).
	return slices.DeleteFunc(list, func(i IfAddr) bool { return len(i.Addrs) == 0 })
}

// GuestInfo is what the VM reports about itself through the guest agent.
type GuestInfo struct {
	OS       string    `json:"os"`
	Kernel   string    `json:"kernel"`
	Hostname string    `json:"hostname"`
	Timezone string    `json:"timezone"`
	FS       []GuestFS `json:"fs"`
}

// GuestFS is a mounted filesystem inside the VM.
type GuestFS struct {
	Mount string `json:"mount"`
	Type  string `json:"type"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

// GuestInfo asks the guest agent about the VM's OS, name and filesystems.
func (c *Client) GuestInfo(ctx context.Context, name string) (GuestInfo, error) {
	out, err := c.run(ctx, "guestinfo", "--os", "--hostname", "--timezone", "--filesystem", "--domain", name)
	if err != nil {
		return GuestInfo{}, err
	}
	return parseGuestInfo(out), nil
}

// Filesystems that aren't real disks (snap packages, live media, RAM).
var pseudoFS = map[string]bool{"squashfs": true, "iso9660": true, "overlay": true, "tmpfs": true, "devtmpfs": true, "udf": true}

func parseGuestInfo(out string) GuestInfo {
	kv := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	g := GuestInfo{
		OS:       firstNonEmpty(kv["os.pretty-name"], strings.TrimSpace(kv["os.name"]+" "+kv["os.version"])),
		Kernel:   kv["os.kernel-release"],
		Hostname: kv["hostname"],
		FS:       []GuestFS{},
	}
	if tz := kv["timezone.name"]; tz != "" {
		g.Timezone = tz
		if off, err := strconv.Atoi(kv["timezone.offset"]); err == nil {
			g.Timezone += fmt.Sprintf(" (UTC%+d", off/3600)
			if m := off % 3600 / 60; m != 0 {
				g.Timezone += fmt.Sprintf(":%02d", max(m, -m))
			}
			g.Timezone += ")"
		}
	}
	n, _ := strconv.Atoi(kv["fs.count"])
	seen := map[string]bool{}
	for i := range n {
		p := fmt.Sprintf("fs.%d.", i)
		total, _ := strconv.ParseUint(kv[p+"total-bytes"], 10, 64)
		used, _ := strconv.ParseUint(kv[p+"used-bytes"], 10, 64)
		mount, typ := kv[p+"mountpoint"], kv[p+"fstype"]
		if total == 0 || pseudoFS[typ] || seen[mount] {
			continue
		}
		seen[mount] = true
		g.FS = append(g.FS, GuestFS{Mount: mount, Type: typ, Total: total, Used: used})
	}
	slices.SortFunc(g.FS, func(a, b GuestFS) int { return strings.Compare(a.Mount, b.Mount) })
	return g
}
