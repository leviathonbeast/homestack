// Package libvirt manages KVM virtual machines through libvirt.
//
// It drives the `virsh` command (the same way Homestack drives
// `docker compose`) against libvirt's socket, so it works with any libvirt
// version and needs no cgo. When running in a container, mount
// /var/run/libvirt from the host.
package libvirt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client runs virsh against one libvirt daemon.
type Client struct {
	uri string

	mu      sync.Mutex
	lastCPU map[string]cpuSample
}

type cpuSample struct {
	ns   uint64
	when time.Time
}

// New uses HOMESTACK_LIBVIRT_URI, or qemu:///system.
func New() *Client {
	uri := os.Getenv("HOMESTACK_LIBVIRT_URI")
	if uri == "" {
		uri = "qemu:///system"
	}
	return &Client{uri: uri, lastCPU: map[string]cpuSample{}}
}

// ErrUnavailable means virsh or the libvirt daemon can't be reached.
var ErrUnavailable = errors.New("libvirt isn't available")

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	return c.runInput(ctx, nil, args...)
}

func (c *Client) runInput(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	if _, err := exec.LookPath("virsh"); err != nil {
		return "", fmt.Errorf("%w: the virsh command isn't installed", ErrUnavailable)
	}
	full := append([]string{"-c", c.uri, "-q"}, args...)
	cmd := exec.CommandContext(ctx, "virsh", full...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		msg = strings.TrimPrefix(msg, "error: ")
		msg = strings.ReplaceAll(msg, "\nerror: ", " ")
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "Failed to connect socket") || strings.Contains(msg, "failed to connect to the hypervisor") {
			return "", fmt.Errorf("%w: %s", ErrUnavailable, msg)
		}
		return "", errors.New(msg)
	}
	return out.String(), nil
}

// Status reports whether VM management can work.
func (c *Client) Status(ctx context.Context) (version string, err error) {
	out, err := c.run(ctx, "version", "--daemon")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "Running hypervisor") {
			return strings.TrimSpace(line[strings.Index(line, ":")+1:]), nil
		}
	}
	return "libvirt", nil
}

// ---------- Domain XML ----------

// DomainXML is the subset of libvirt's domain XML Homestack reads.
type DomainXML struct {
	XMLName     xml.Name `xml:"domain"`
	Type        string   `xml:"type,attr"`
	Name        string   `xml:"name"`
	UUID        string   `xml:"uuid"`
	Description string   `xml:"description"`
	Memory      struct {
		Value uint64 `xml:",chardata"`
		Unit  string `xml:"unit,attr"`
	} `xml:"memory"`
	VCPU struct {
		Value int `xml:",chardata"`
	} `xml:"vcpu"`
	OS struct {
		Firmware string `xml:"firmware,attr"`
		Type     struct {
			Machine string `xml:"machine,attr"`
		} `xml:"type"`
		Loader struct {
			Value string `xml:",chardata"`
			Type  string `xml:"type,attr"`
		} `xml:"loader"`
	} `xml:"os"`
	Metadata struct {
		// Unraid stores the OS it was created as here.
		Unraid struct {
			OS   string `xml:"os,attr"`
			Name string `xml:"name,attr"`
		} `xml:"vmtemplate"`
		Homestack struct {
			OS       string `xml:"os,attr"`
			Template string `xml:"template,attr"`
		} `xml:"vm"`
	} `xml:"metadata"`
	Devices struct {
		Disks []struct {
			Type   string `xml:"type,attr"`
			Device string `xml:"device,attr"`
			Driver struct {
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File string `xml:"file,attr"`
				Dev  string `xml:"dev,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
				Bus string `xml:"bus,attr"`
			} `xml:"target"`
		} `xml:"disk"`
		Interfaces []struct {
			Type string `xml:"type,attr"`
			MAC  struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Source struct {
				Bridge  string `xml:"bridge,attr"`
				Network string `xml:"network,attr"`
				Dev     string `xml:"dev,attr"`
			} `xml:"source"`
			Model struct {
				Type string `xml:"type,attr"`
			} `xml:"model"`
		} `xml:"interface"`
		Graphics []struct {
			Type    string `xml:"type,attr"`
			Port    int    `xml:"port,attr"`
			Listen  string `xml:"listen,attr"`
			Passwd  string `xml:"passwd,attr"`
			Listens []struct {
				Type    string `xml:"type,attr"`
				Address string `xml:"address,attr"`
			} `xml:"listen"`
		} `xml:"graphics"`
		Hostdevs []struct{} `xml:"hostdev"`
		Serials  []struct {
			Type string `xml:"type,attr"`
		} `xml:"serial"`
		Consoles []struct {
			Type string `xml:"type,attr"`
		} `xml:"console"`
		Channels []struct {
			Target struct {
				Name  string `xml:"name,attr"`
				State string `xml:"state,attr"` // live XML: connected | disconnected
			} `xml:"target"`
		} `xml:"channel"`
	} `xml:"devices"`
}

func toBytes(v uint64, unit string) uint64 {
	switch strings.ToLower(unit) {
	case "b", "bytes":
		return v
	case "k", "kib", "":
		return v * 1024
	case "kb":
		return v * 1000
	case "m", "mib":
		return v << 20
	case "mb":
		return v * 1000 * 1000
	case "g", "gib":
		return v << 30
	case "gb":
		return v * 1000 * 1000 * 1000
	}
	return v * 1024
}

// ---------- VM summaries ----------

type Disk struct {
	Target string `json:"target"`
	Device string `json:"device"` // disk | cdrom
	Path   string `json:"path"`
	Format string `json:"format"`
	Bus    string `json:"bus"`
}

type NIC struct {
	MAC    string `json:"mac"`
	Source string `json:"source"` // bridge or network name
	Kind   string `json:"kind"`   // bridge | network | direct
	Model  string `json:"model"`
}

type VM struct {
	Name        string   `json:"name"`
	UUID        string   `json:"uuid"`
	State       string   `json:"state"` // running, paused, shutoff, crashed, suspended, shutting-down
	Autostart   bool     `json:"autostart"`
	VCPUs       int      `json:"vcpus"`
	Memory      uint64   `json:"memory"`  // assigned, bytes
	MemoryUsed  uint64   `json:"memUsed"` // bytes: host memory the VM process uses, or what programs in it use when GuestMem
	GuestMem    bool     `json:"guestMem,omitempty"`
	CPU         float64  `json:"cpu"`      // percent of its vCPUs
	Firmware    string   `json:"firmware"` // uefi | bios
	Machine     string   `json:"machine"`
	OS          string   `json:"os"` // hint for the icon: linux, windows, debian...
	Description string   `json:"description"`
	Disks       []Disk   `json:"disks"`
	NICs        []NIC    `json:"nics"`
	Console     string   `json:"console"`         // vnc, spice or ""
	Serial      bool     `json:"serial"`          // has a serial port or console, for the Serial tab
	Agent       string   `json:"agent"`           // QEMU guest agent: "" (no channel), connected, disconnected
	Addrs       []string `json:"addrs,omitempty"` // its addresses, when known (filled in by the server)
	Passthrough int      `json:"passthrough"`
	Template    string   `json:"template,omitempty"` // the template it was cloned from
}

var stateNames = map[string]string{
	"0": "unknown", "1": "running", "2": "running", "3": "paused",
	"4": "shutting-down", "5": "shutoff", "6": "crashed", "7": "suspended",
}

// domstats parses `virsh domstats --raw` into name -> key -> value.
func (c *Client) domstats(ctx context.Context, flags ...string) (map[string]map[string]string, []string, error) {
	out, err := c.run(ctx, append([]string{"domstats", "--raw"}, flags...)...)
	if err != nil {
		return nil, nil, err
	}
	stats := map[string]map[string]string{}
	var order []string
	var cur map[string]string
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if after, ok := strings.CutPrefix(line, "Domain: "); ok {
			name := strings.Trim(after, "'")
			cur = map[string]string{}
			stats[name] = cur
			order = append(order, name)
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && cur != nil {
			cur[k] = v
		}
	}
	return stats, order, nil
}

func (c *Client) autostartSet(ctx context.Context) map[string]bool {
	out, err := c.run(ctx, "list", "--all", "--autostart", "--name")
	set := map[string]bool{}
	if err != nil {
		return set
	}
	for n := range strings.SplitSeq(out, "\n") {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}

// DumpXML returns a domain's XML. inactive gives the saved config rather
// than the live one.
func (c *Client) DumpXML(ctx context.Context, name string, inactive bool) (string, error) {
	args := []string{"dumpxml", "--security-info"}
	if inactive {
		args = append(args, "--inactive")
	}
	return c.run(ctx, append(args, "--", name)...)
}

func summarize(d DomainXML) VM {
	vm := VM{
		Name: d.Name, UUID: d.UUID, VCPUs: d.VCPU.Value, Machine: d.OS.Type.Machine,
		Memory: toBytes(d.Memory.Value, d.Memory.Unit), Description: strings.TrimSpace(d.Description),
		Disks: []Disk{}, NICs: []NIC{}, Passthrough: len(d.Devices.Hostdevs),
	}
	vm.Firmware = "bios"
	if d.OS.Firmware == "efi" || d.OS.Loader.Type == "pflash" || strings.Contains(strings.ToLower(d.OS.Loader.Value), "ovmf") {
		vm.Firmware = "uefi"
	}
	vm.OS = firstNonEmpty(d.Metadata.Homestack.OS, d.Metadata.Unraid.OS)
	vm.Template = d.Metadata.Homestack.Template
	for _, dk := range d.Devices.Disks {
		vm.Disks = append(vm.Disks, Disk{
			Target: dk.Target.Dev, Device: dk.Device, Path: firstNonEmpty(dk.Source.File, dk.Source.Dev),
			Format: dk.Driver.Type, Bus: dk.Target.Bus,
		})
	}
	for _, n := range d.Devices.Interfaces {
		vm.NICs = append(vm.NICs, NIC{
			MAC: n.MAC.Address, Kind: n.Type, Model: n.Model.Type,
			Source: firstNonEmpty(n.Source.Bridge, n.Source.Network, n.Source.Dev),
		})
	}
	vm.Serial = len(d.Devices.Serials) > 0 || len(d.Devices.Consoles) > 0
	for _, ch := range d.Devices.Channels {
		if ch.Target.Name == "org.qemu.guest_agent.0" {
			vm.Agent = firstNonEmpty(ch.Target.State, "disconnected")
		}
	}
	for _, g := range d.Devices.Graphics {
		if g.Type == "vnc" || g.Type == "spice" {
			vm.Console = g.Type
			if g.Type == "vnc" {
				break
			}
		}
	}
	return vm
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// List returns every VM with live CPU and memory figures.
func (c *Client) List(ctx context.Context) ([]VM, error) {
	stats, names, err := c.domstats(ctx, "--state", "--cpu-total", "--balloon")
	if err != nil {
		return nil, err
	}
	auto := c.autostartSet(ctx)
	now := time.Now()
	out := make([]VM, 0, len(names))
	for _, name := range names {
		raw, err := c.DumpXML(ctx, name, false)
		if err != nil {
			continue // removed in the meantime
		}
		var d DomainXML
		if xml.Unmarshal([]byte(raw), &d) != nil {
			continue
		}
		vm := summarize(d)
		st := stats[name]
		vm.State = stateNames[st["state.state"]]
		if vm.State == "" {
			vm.State = "unknown"
		}
		vm.Autostart = auto[name]
		if rss, err := strconv.ParseUint(st["balloon.rss"], 10, 64); err == nil && vm.State == "running" {
			vm.MemoryUsed = rss * 1024
		}
		if ns, err := strconv.ParseUint(st["cpu.time"], 10, 64); err == nil && vm.State == "running" {
			c.mu.Lock()
			prev, ok := c.lastCPU[vm.UUID]
			c.lastCPU[vm.UUID] = cpuSample{ns, now}
			c.mu.Unlock()
			if ok && ns >= prev.ns && vm.VCPUs > 0 {
				elapsed := now.Sub(prev.when).Nanoseconds()
				if elapsed > 0 {
					vm.CPU = float64(ns-prev.ns) / float64(elapsed) / float64(vm.VCPUs) * 100
					if vm.CPU > 100 {
						vm.CPU = 100
					}
				}
			}
		}
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// Get returns one VM.
func (c *Client) Get(ctx context.Context, name string) (*VM, error) {
	list, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("there's no VM called %s", name)
}

// ---------- Actions ----------

var actions = map[string][]string{
	"start":    {"start"},
	"shutdown": {"shutdown"},
	"stop":     {"destroy"}, // force off, like pulling the plug
	"reboot":   {"reboot"},
	"pause":    {"suspend"},
	"resume":   {"resume"},
	"reset":    {"reset"},
}

// Action runs a power action.
func (c *Client) Action(ctx context.Context, name, action string) error {
	args, ok := actions[action]
	if !ok {
		return fmt.Errorf("unknown action %q", action)
	}
	_, err := c.run(ctx, append(args, "--", name)...)
	return err
}

// SetAutostart turns starting with the host on or off.
func (c *Client) SetAutostart(ctx context.Context, name string, on bool) error {
	args := []string{"autostart"}
	if !on {
		args = append(args, "--disable")
	}
	_, err := c.run(ctx, append(args, "--", name)...)
	return err
}

// Define saves a domain from XML (creating or updating it).
func (c *Client) Define(ctx context.Context, domainXML string) error {
	f, err := os.CreateTemp("", "homestack-vm-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(domainXML); err != nil {
		f.Close()
		return err
	}
	f.Close()
	_, err = c.run(ctx, "define", "--validate", f.Name())
	return err
}

// Undefine removes a VM definition (and its UEFI variables). Disks are
// deleted only when deleteDisks is set.
func (c *Client) Undefine(ctx context.Context, name string, deleteDisks bool) error {
	vm, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	if vm.State != "shutoff" && vm.State != "crashed" {
		return errors.New("stop the VM before deleting it")
	}
	if _, err := c.run(ctx, "undefine", "--nvram", "--", name); err != nil {
		if _, err2 := c.run(ctx, "undefine", "--", name); err2 != nil {
			return err
		}
	}
	if !deleteDisks {
		return nil
	}
	var failed []string
	for _, d := range vm.Disks {
		// The cloud-init disk Homestack made for it goes too; other CDs (installers) stay.
		dir, file := splitPath(d.Path)
		seed := d.Device == "cdrom" && file == SeedFile && strings.HasSuffix(dir, "/"+name)
		if !seed && (d.Device != "disk" || d.Path == "" || strings.HasPrefix(d.Path, "/dev/")) {
			continue
		}
		if err := c.DeleteFile(ctx, d.Path); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", d.Path, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the VM was deleted, but these disks couldn't be: %s", strings.Join(failed, "; "))
	}
	// Tidy up the VM's own folder (e.g. /mnt/user/domains/<name>) if it's now empty.
	for _, d := range vm.Disks {
		if dir, _ := splitPath(d.Path); d.Device == "disk" && strings.HasSuffix(dir, "/"+name) {
			c.removeEmptyDir(ctx, dir)
		}
	}
	return nil
}

// removeEmptyDir deletes a folder through libvirt, only if it's empty.
func (c *Client) removeEmptyDir(ctx context.Context, dir string) {
	name := fmt.Sprintf("homestack-rm-%d", time.Now().UnixNano())
	if _, err := c.run(ctx, "pool-define-as", name, "dir", "--target", dir); err != nil {
		return
	}
	_, _ = c.run(ctx, "pool-delete", "--", name) // refuses if the folder isn't empty
	_, _ = c.run(ctx, "pool-undefine", "--", name)
}

// ---------- Capabilities ----------

// Caps is what the host can offer a new VM.
type Caps struct {
	DomainType string   `json:"domainType"` // kvm, or qemu when KVM isn't available
	Emulator   string   `json:"emulator"`
	Machine    string   `json:"machine"`
	Loaders    []string `json:"loaders"` // UEFI firmware files
	TPM        bool     `json:"tpm"`
}

type domcaps struct {
	Path    string   `xml:"path"`
	Domain  string   `xml:"domain"`
	Machine string   `xml:"machine"`
	Loader  []string `xml:"os>loader>value"`
	TPM     struct {
		Supported string   `xml:"supported,attr"`
		Backends  []string `xml:"enum>value"`
	} `xml:"devices>tpm"`
}

// Capabilities asks libvirt what it supports, trying KVM first.
func (c *Client) Capabilities(ctx context.Context) (Caps, error) {
	var lastErr error
	for _, vt := range []string{"kvm", "qemu"} {
		out, err := c.run(ctx, "domcapabilities", "--virttype", vt, "--arch", "x86_64", "--machine", "q35")
		if err != nil {
			lastErr = err
			continue
		}
		var dc domcaps
		if err := xml.Unmarshal([]byte(out), &dc); err != nil {
			lastErr = err
			continue
		}
		caps := Caps{DomainType: firstNonEmpty(dc.Domain, vt), Emulator: dc.Path, Machine: firstNonEmpty(dc.Machine, "q35")}
		for _, l := range dc.Loader {
			low := strings.ToLower(l)
			if strings.Contains(low, "secboot") || strings.Contains(low, ".ms.") || strings.Contains(low, "snakeoil") ||
				strings.Contains(low, "csm") || strings.Contains(low, "amdsev") || strings.Contains(low, "inteltdx") || strings.Contains(low, "vars") {
				continue // plain UEFI only; Secure Boot variants need matching VARS files
			}
			caps.Loaders = append(caps.Loaders, l)
		}
		// Prefer plain OVMF_CODE builds, and the 4 MB one (the default on current distros).
		score := func(p string) int {
			n := 0
			if strings.Contains(p, "OVMF_CODE") {
				n += 2
			}
			if strings.Contains(p, "4M") {
				n++
			}
			return n
		}
		sort.SliceStable(caps.Loaders, func(i, j int) bool { return score(caps.Loaders[i]) > score(caps.Loaders[j]) })
		caps.TPM = dc.TPM.Supported == "yes"
		return caps, nil
	}
	return Caps{}, lastErr
}

// ---------- Storage helpers ----------

// withDirPool runs fn with a libvirt storage pool covering dir. It reuses an
// existing pool for that folder, or creates a temporary one that disappears
// afterwards. This lets Homestack create and delete disk images through
// libvirt, without needing the folder mounted into its own container.
func (c *Client) withDirPool(ctx context.Context, dir string, fn func(pool string) error) error {
	dir = strings.TrimRight(dir, "/")
	if out, err := c.run(ctx, "pool-list", "--all", "--name"); err == nil {
		for p := range strings.SplitSeq(out, "\n") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			x, err := c.run(ctx, "pool-dumpxml", "--", p)
			if err == nil && strings.Contains(x, "<path>"+dir+"</path>") {
				_, _ = c.run(ctx, "pool-start", "--", p) // no-op if already running
				_, _ = c.run(ctx, "pool-refresh", "--", p)
				return fn(p)
			}
		}
	}
	name := fmt.Sprintf("homestack-tmp-%d", time.Now().UnixNano())
	if _, err := c.run(ctx, "pool-create-as", name, "dir", "--target", dir, "--build"); err != nil {
		return err
	}
	defer func() { _, _ = c.run(context.Background(), "pool-destroy", "--", name) }()
	return fn(name)
}

// CreateDisk makes a new qcow2 image.
func (c *Client) CreateDisk(ctx context.Context, path string, sizeGiB int) error {
	dir, file := splitPath(path)
	return c.withDirPool(ctx, dir, func(pool string) error {
		if _, err := c.run(ctx, "vol-info", "--pool", pool, file); err == nil {
			return fmt.Errorf("%s already exists", path)
		}
		_, err := c.run(ctx, "vol-create-as", pool, file, fmt.Sprintf("%dG", sizeGiB), "--format", "qcow2")
		return err
	})
}

// DeleteFile removes a disk image through libvirt.
func (c *Client) DeleteFile(ctx context.Context, path string) error {
	dir, file := splitPath(path)
	return c.withDirPool(ctx, dir, func(pool string) error {
		_, err := c.run(ctx, "vol-delete", "--pool", pool, file)
		return err
	})
}

// FileExists checks for a file through libvirt.
func (c *Client) FileExists(ctx context.Context, path string) bool {
	dir, file := splitPath(path)
	found := false
	_ = c.withDirPool(ctx, dir, func(pool string) error {
		_, err := c.run(ctx, "vol-info", "--pool", pool, file)
		found = err == nil
		return nil
	})
	return found
}

func splitPath(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/", strings.TrimPrefix(p, "/")
	}
	return p[:i], p[i+1:]
}

// ---------- Console ----------

// SerialCommand is `virsh console` for a VM's serial console. virsh only
// runs it on a terminal, so the caller starts it on a pseudo-terminal.
// --safe refuses unless libvirt can guarantee one session at a time;
// force takes the console over from whoever has it open.
func (c *Client) SerialCommand(ctx context.Context, name string, force bool) *exec.Cmd {
	args := []string{"-c", c.uri, "console", "--safe"}
	if force {
		args = append(args, "--force")
	}
	cmd := exec.CommandContext(ctx, "virsh", append(args, "--domain", name)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TERM=xterm-256color")
	return cmd
}

// ConsoleAddr returns where a running VM's VNC server listens, and its
// password if it has one.
func (c *Client) ConsoleAddr(ctx context.Context, name string) (addr, password string, err error) {
	raw, err := c.DumpXML(ctx, name, false)
	if err != nil {
		return "", "", err
	}
	var d DomainXML
	if err := xml.Unmarshal([]byte(raw), &d); err != nil {
		return "", "", err
	}
	for _, g := range d.Devices.Graphics {
		if g.Type != "vnc" {
			continue
		}
		if g.Port <= 0 {
			return "", "", errors.New("the VM isn't running, or its console isn't ready yet")
		}
		host := g.Listen
		for _, l := range g.Listens {
			if l.Type == "address" && l.Address != "" {
				host = l.Address
			}
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return fmt.Sprintf("%s:%d", host, g.Port), g.Passwd, nil
	}
	return "", "", errors.New("this VM has no VNC console. Add one in its XML: <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>")
}

// Networks lists libvirt's own virtual networks (e.g. "default", a NAT network).
func (c *Client) Networks(ctx context.Context) []string {
	out, err := c.run(ctx, "net-list", "--all", "--name")
	res := []string{}
	if err != nil {
		return res
	}
	for n := range strings.SplitSeq(out, "\n") {
		if n = strings.TrimSpace(n); n != "" {
			res = append(res, n)
		}
	}
	return res
}
