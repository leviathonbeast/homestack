package libvirt

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The VM settings form: the parts of a definition most people change
// (CPUs, memory, disks and CDs, network, display, USB devices, boot order),
// edited without touching anything else in the XML.

// VMConfig is what the form shows and sends back.
type VMConfig struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	VCPUs       int          `json:"vcpus"`
	MemoryMB    int          `json:"memoryMB"`
	Disks       []DiskConfig `json:"disks"`
	NICs        []NICConfig  `json:"nics"`
	Boot        string       `json:"boot"` // disk | cdrom: which boots first
	VNC         bool         `json:"vnc"`
	Serial      *bool        `json:"serial,omitempty"` // a serial port, for the Serial tab (missing: leave as is)
	Video       string       `json:"video"`            // virtio, vga, qxl, bochs, cirrus, none
	USB         []USBDevice  `json:"usb"`

	// Shown, not changed here.
	Firmware string   `json:"firmware"`
	Machine  string   `json:"machine"`
	CPUMode  string   `json:"cpuMode"`
	Kept     []string `json:"kept"` // other devices, kept as they are
}

// DiskConfig is one disk or CD drive.
type DiskConfig struct {
	Target    string `json:"target"` // its device name now ("" = add a new one)
	Device    string `json:"device"` // disk | cdrom
	Source    string `json:"source"` // file path; "" on a CD = empty drive
	Bus       string `json:"bus"`    // virtio, sata, scsi, usb, ide
	Format    string `json:"format,omitempty"`
	Size      int64  `json:"size,omitempty"`      // bytes, existing file disks
	NewSizeGB int    `json:"newSizeGB,omitempty"` // new disk: create this big; existing: grow to this
	Fixed     bool   `json:"fixed,omitempty"`     // a block device or network disk: only removable here
	Remove    bool   `json:"remove,omitempty"`
}

// NICConfig is one network card.
type NICConfig struct {
	Index  int    `json:"index"` // its position among the VM's cards, from 1 (0 = add a new one)
	MAC    string `json:"mac"`
	Kind   string `json:"kind"` // bridge | network (others are kept as they are)
	Source string `json:"source"`
	Model  string `json:"model"`
	Fixed  bool   `json:"fixed,omitempty"`
	Remove bool   `json:"remove,omitempty"`
}

// USBDevice is a host USB device passed to the VM, by vendor and product.
type USBDevice struct {
	Vendor  string `json:"vendor"`  // 0x1234
	Product string `json:"product"` // 0x5678
	Name    string `json:"name,omitempty"`
}

var (
	diskBuses = []string{"virtio", "sata", "scsi", "usb", "ide"}
	cdBuses   = []string{"sata", "scsi", "usb", "ide"}
	nicModels = []string{"virtio", "e1000e", "e1000", "rtl8139", "vmxnet3"}
	videos    = []string{"virtio", "vga", "qxl", "bochs", "cirrus", "none"}
	reUSBID   = regexp.MustCompile(`^0x[0-9a-fA-F]{4}$`)
)

// ReadConfig fills the form from a domain's XML.
func ReadConfig(domainXML string) (VMConfig, error) {
	root, err := ParseDoc(domainXML)
	if err != nil {
		return VMConfig{}, err
	}
	var d DomainXML
	if err := xml.Unmarshal([]byte(domainXML), &d); err != nil {
		return VMConfig{}, err
	}
	vm := summarize(d)
	cfg := VMConfig{
		Name: vm.Name, Description: vm.Description, VCPUs: vm.VCPUs, MemoryMB: int(vm.Memory >> 20),
		Firmware: vm.Firmware, Machine: vm.Machine, CPUMode: root.Path("cpu").Attr("mode"),
		Disks: []DiskConfig{}, NICs: []NICConfig{}, USB: []USBDevice{}, Kept: []string{}, Boot: "disk",
	}
	devs := root.Child("devices")
	firstBoot := map[string]int{}
	for _, dk := range devs.Children("disk") {
		dc := DiskConfig{
			Target: dk.Child("target").Attr("dev"), Device: dk.Attr("device"), Bus: dk.Child("target").Attr("bus"),
			Format: dk.Child("driver").Attr("type"), Source: dk.Child("source").Attr("file"),
		}
		if dc.Device == "" {
			dc.Device = "disk"
		}
		if t := dk.Attr("type"); t != "file" && t != "" {
			dc.Fixed = true
			dc.Source = firstNonEmpty(dk.Child("source").Attr("dev"), dk.Child("source").Attr("name"), t+" disk")
		}
		if dc.Device != "disk" && dc.Device != "cdrom" {
			dc.Fixed = true
		}
		if o, err := strconv.Atoi(dk.Child("boot").Attr("order")); err == nil {
			kind := map[bool]string{true: "cdrom", false: "disk"}[dc.Device == "cdrom"]
			if cur, ok := firstBoot[kind]; !ok || o < cur {
				firstBoot[kind] = o
			}
		}
		cfg.Disks = append(cfg.Disks, dc)
	}
	if c, okc := firstBoot["cdrom"]; okc {
		if dsk, okd := firstBoot["disk"]; !okd || c < dsk {
			cfg.Boot = "cdrom"
		}
	}
	if boots := root.Path("os").Children("boot"); len(boots) > 0 { // the older <os><boot dev=…/> style
		cfg.Boot = map[bool]string{true: "cdrom", false: "disk"}[boots[0].Attr("dev") == "cdrom"]
	}
	for i, n := range devs.Children("interface") {
		nc := NICConfig{Index: i + 1, MAC: n.Child("mac").Attr("address"), Kind: n.Attr("type"), Model: n.Child("model").Attr("type")}
		switch nc.Kind {
		case "bridge":
			nc.Source = n.Child("source").Attr("bridge")
		case "network":
			nc.Source = n.Child("source").Attr("network")
		default:
			nc.Fixed = true
			nc.Source = firstNonEmpty(n.Child("source").Attr("dev"), nc.Kind)
		}
		cfg.NICs = append(cfg.NICs, nc)
	}
	for _, g := range devs.Children("graphics") {
		if g.Attr("type") == "vnc" {
			cfg.VNC = true
		}
	}
	hasSerial := devs.Child("serial") != nil || devs.Child("console") != nil
	cfg.Serial = &hasSerial
	cfg.Video = firstNonEmpty(devs.Path("video", "model").Attr("type"), "none")
	pci, other := 0, 0
	for _, h := range devs.Children("hostdev") {
		src := h.Child("source")
		switch {
		case h.Attr("type") == "usb" && src.Child("vendor") != nil:
			cfg.USB = append(cfg.USB, USBDevice{Vendor: src.Child("vendor").Attr("id"), Product: src.Child("product").Attr("id")})
		case h.Attr("type") == "pci":
			pci++
		default:
			other++
		}
	}
	if pci > 0 {
		cfg.Kept = append(cfg.Kept, plural(pci, "PCI device")+" passed through (a graphics card, controller…)")
	}
	if other > 0 {
		cfg.Kept = append(cfg.Kept, plural(other, "other passed-through device"))
	}
	if n := len(root.Path("cputune").Children("vcpupin")); n > 0 {
		cfg.Kept = append(cfg.Kept, "CPU pinning ("+plural(n, "vCPU")+" pinned to host threads)")
	}
	for _, name := range []string{"sound", "filesystem", "smartcard", "redirdev"} {
		if n := len(devs.Children(name)); n > 0 {
			cfg.Kept = append(cfg.Kept, plural(n, name))
		}
	}
	return cfg, nil
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// ApplyConfig changes a domain's XML to match the form. New disks must
// already have a Source (SaveConfig picks their paths).
func ApplyConfig(domainXML string, cfg VMConfig) (string, error) {
	root, err := ParseDoc(domainXML)
	if err != nil {
		return "", err
	}
	if root.Child("name").Text() != cfg.Name {
		return "", errors.New("the VM's name can't be changed here")
	}
	if cfg.VCPUs < 1 || cfg.VCPUs > 256 {
		return "", errors.New("pick between 1 and 256 vCPUs")
	}
	if cfg.MemoryMB < 128 {
		return "", errors.New("give the VM at least 128 MB of memory")
	}

	// Description
	if desc := strings.TrimSpace(cfg.Description); desc != "" {
		d := root.Child("description")
		if d == nil {
			d = root.Add(El("description"), root.Child("name"))
		}
		d.SetText(desc)
	} else if d := root.Child("description"); d != nil {
		root.Remove(d)
	}

	// CPUs: keep the topology and pinning consistent with the new count.
	vc := root.Ensure("vcpu")
	vc.SetText(strconv.Itoa(cfg.VCPUs))
	if cur, err := strconv.Atoi(vc.Attr("current")); err == nil && cur > cfg.VCPUs {
		vc.DelAttr("current")
	}
	if topo := root.Path("cpu", "topology"); topo != nil {
		threads, _ := strconv.Atoi(topo.Attr("threads"))
		if threads < 1 || cfg.VCPUs%threads != 0 {
			threads = 1
		}
		topo.SetAttr("sockets", "1")
		if topo.Attr("dies") != "" {
			topo.SetAttr("dies", "1")
		}
		if topo.Attr("clusters") != "" {
			topo.SetAttr("clusters", "1")
		}
		topo.SetAttr("cores", strconv.Itoa(cfg.VCPUs/threads))
		topo.SetAttr("threads", strconv.Itoa(threads))
	}
	if ct := root.Child("cputune"); ct != nil {
		for _, pin := range ct.Children("vcpupin") {
			if n, err := strconv.Atoi(pin.Attr("vcpu")); err == nil && n >= cfg.VCPUs {
				ct.Remove(pin)
			}
		}
	}

	// Memory
	for _, name := range []string{"memory", "currentMemory"} {
		m := root.Child(name)
		if m == nil && name == "memory" {
			m = root.Add(El("memory"), vc)
		}
		if m != nil {
			m.SetAttr("unit", "MiB")
			m.SetText(strconv.Itoa(cfg.MemoryMB))
		}
	}

	devs := root.Ensure("devices")
	if err := applyDisks(devs, cfg); err != nil {
		return "", err
	}
	if err := applyNICs(devs, cfg.NICs); err != nil {
		return "", err
	}
	applyBoot(root, devs, cfg.Boot)
	if err := applyDisplay(devs, cfg); err != nil {
		return "", err
	}
	if cfg.Serial != nil {
		applySerial(devs, *cfg.Serial)
	}
	if err := applyUSB(devs, cfg.USB); err != nil {
		return "", err
	}
	return root.String(), nil
}

var devPrefix = map[string]string{"virtio": "vd", "sata": "sd", "scsi": "sd", "usb": "sd", "ide": "hd"}

// nextDev picks a free device name for a bus: vda, vdb… or sda, sdb…
func nextDev(bus string, used map[string]bool) string {
	p := devPrefix[bus]
	for c := 'a'; c <= 'z'; c++ {
		if d := p + string(c); !used[d] {
			used[d] = true
			return d
		}
	}
	for c := 'a'; c <= 'z'; c++ {
		for c2 := 'a'; c2 <= 'z'; c2++ {
			if d := p + string(c) + string(c2); !used[d] {
				used[d] = true
				return d
			}
		}
	}
	return ""
}

func applyDisks(devs *Node, cfg VMConfig) error {
	byTarget := map[string]*Node{}
	for _, dk := range devs.Children("disk") {
		byTarget[dk.Child("target").Attr("dev")] = dk
	}
	used := map[string]bool{}
	for _, dc := range cfg.Disks {
		if dc.Target != "" && !dc.Remove {
			used[dc.Target] = true
		}
	}
	needSCSI := false
	var last *Node
	for _, dk := range devs.Children("disk") {
		last = dk
	}
	for _, dc := range cfg.Disks {
		cd := dc.Device == "cdrom"
		if dc.Target != "" {
			dk := byTarget[dc.Target]
			if dk == nil {
				return fmt.Errorf("there's no disk %s any more; reload the page", dc.Target)
			}
			if dc.Remove {
				devs.Remove(dk)
				continue
			}
			if dc.Fixed || (dk.Attr("type") != "file" && dk.Attr("type") != "") {
				continue
			}
			// Only what the form changed has to be one of the usual values.
			check := dc
			if dc.Bus == dk.Child("target").Attr("bus") {
				check.Bus = map[bool]string{true: "sata", false: "virtio"}[cd]
			}
			if dc.Source == dk.Child("source").Attr("file") {
				check.Source = ""
			}
			if err := checkDisk(check); err != nil {
				return err
			}
			tgt := dk.Ensure("target")
			if bus := tgt.Attr("bus"); bus != dc.Bus {
				delete(used, dc.Target)
				tgt.SetAttr("bus", dc.Bus)
				tgt.SetAttr("dev", nextDev(dc.Bus, used))
				if a := dk.Child("address"); a != nil {
					dk.Remove(a) // libvirt places it on the new bus
				}
			}
			setSource(dk, dc.Source, cd)
		} else if !dc.Remove {
			if err := checkDisk(dc); err != nil {
				return err
			}
			if dc.Source == "" && !cd {
				return errors.New("a new disk needs a size or the path of an existing image")
			}
			dk := El("disk", "type", "file", "device", dc.Device)
			if cd {
				dk.With(El("driver", "name", "qemu", "type", "raw"))
			} else {
				dk.With(El("driver", "name", "qemu", "type", diskFormat(dc.Source), "cache", "writeback", "discard", "unmap"))
			}
			if dc.Source != "" {
				dk.With(El("source", "file", dc.Source))
			}
			dk.With(El("target", "dev", nextDev(dc.Bus, used), "bus", dc.Bus))
			if cd {
				dk.With(El("readonly"))
			}
			last = devs.Add(dk, last)
		}
		if dc.Bus == "scsi" && !dc.Remove {
			needSCSI = true
		}
	}
	if needSCSI {
		has := false
		for _, c := range devs.Children("controller") {
			if c.Attr("type") == "scsi" {
				has = true
			}
		}
		if !has {
			devs.Add(El("controller", "type", "scsi", "model", "virtio-scsi"), last)
		}
	}
	return nil
}

func checkDisk(dc DiskConfig) error {
	if dc.Device == "cdrom" {
		if !slices.Contains(cdBuses, dc.Bus) {
			return fmt.Errorf("a CD drive can't use the %s bus; pick SATA", dc.Bus)
		}
	} else if dc.Device == "disk" {
		if !slices.Contains(diskBuses, dc.Bus) {
			return fmt.Errorf("unknown disk bus %q", dc.Bus)
		}
	} else {
		return fmt.Errorf("unknown device type %q", dc.Device)
	}
	if dc.Source != "" && (!filepath.IsAbs(dc.Source) || strings.ContainsAny(dc.Source, "\x00\n")) {
		return fmt.Errorf("use a full path for %q", dc.Source)
	}
	return nil
}

func diskFormat(path string) string {
	low := strings.ToLower(path)
	if strings.HasSuffix(low, ".img") || strings.HasSuffix(low, ".raw") {
		return "raw"
	}
	return "qcow2"
}

// setSource points a disk at a file; an empty path ejects a CD.
func setSource(dk *Node, path string, cd bool) {
	src := dk.Child("source")
	if path == "" {
		if cd && src != nil {
			dk.Remove(src)
		}
		return
	}
	if src == nil {
		src = dk.Add(El("source"), dk.Child("driver"))
	}
	if src.Attr("file") != path {
		src.Attrs = nil // drop old attributes such as startupPolicy for a different file
		src.SetAttr("file", path)
		if !cd {
			if drv := dk.Child("driver"); drv != nil {
				drv.SetAttr("type", diskFormat(path))
			}
		}
	}
}

func applyNICs(devs *Node, nics []NICConfig) error {
	existing := devs.Children("interface")
	var last *Node
	if len(existing) > 0 {
		last = existing[len(existing)-1]
	} else if ds := devs.Children("disk"); len(ds) > 0 {
		last = ds[len(ds)-1]
	}
	for _, nc := range nics {
		var n *Node
		if nc.Index > 0 {
			// Cards are matched by position (a new card has no MAC address
			// until libvirt saves it), and the MAC double-checks it.
			if nc.Index > len(existing) || (nc.MAC != "" && !strings.EqualFold(existing[nc.Index-1].Child("mac").Attr("address"), nc.MAC)) {
				return errors.New("the network cards changed since this page loaded; reload it")
			}
			n = existing[nc.Index-1]
			if nc.Remove {
				devs.Remove(n)
				continue
			}
			if nc.Fixed || (n.Attr("type") != "bridge" && n.Attr("type") != "network") {
				continue
			}
		} else if nc.Remove {
			continue
		}
		if nc.Kind != "bridge" && nc.Kind != "network" {
			return fmt.Errorf("pick a bridge or a libvirt network for each network card")
		}
		if strings.TrimSpace(nc.Source) == "" || strings.ContainsAny(nc.Source, " /'\"<>&") {
			return fmt.Errorf("pick which network each card connects to")
		}
		if !slices.Contains(nicModels, nc.Model) && (n == nil || n.Child("model").Attr("type") != nc.Model) {
			return fmt.Errorf("unknown network card model %q", nc.Model) // a model already set (like Unraid's virtio-net) is kept
		}
		if n == nil {
			n = devs.Add(El("interface", "type", nc.Kind).With(El("source", nc.Kind, nc.Source), El("model", "type", nc.Model)), last)
			last = n
			continue
		}
		if n.Attr("type") != nc.Kind || n.Child("source").Attr(nc.Kind) != nc.Source {
			n.SetAttr("type", nc.Kind)
			src := n.Child("source")
			if src == nil {
				src = n.Add(El("source"), n.Child("mac"))
			}
			src.Attrs = nil
			src.SetAttr(nc.Kind, nc.Source)
		}
		if m := n.Child("model"); m == nil {
			n.Add(El("model", "type", nc.Model), nil)
		} else if m.Attr("type") != nc.Model {
			m.SetAttr("type", nc.Model)
		}
	}
	return nil
}

// applyBoot makes the first disk or the first CD boot first, using per-device
// boot order (libvirt doesn't allow mixing it with <os><boot dev=…/>).
func applyBoot(root, devs *Node, boot string) {
	if osn := root.Child("os"); osn != nil {
		for _, b := range osn.Children("boot") {
			osn.Remove(b)
		}
	}
	var first, second *Node
	disks, cds := []*Node{}, []*Node{}
	for _, dk := range devs.Children("disk") {
		if b := dk.Child("boot"); b != nil {
			dk.Remove(b)
		}
		switch dk.Attr("device") {
		case "cdrom":
			cds = append(cds, dk)
		case "disk", "":
			disks = append(disks, dk)
		}
	}
	if len(disks) > 0 {
		first = disks[0]
	}
	if len(cds) > 0 {
		second = cds[0]
	}
	if boot == "cdrom" {
		first, second = second, first
	}
	order := 1
	for _, n := range []*Node{first, second} {
		if n != nil {
			n.Add(El("boot", "order", strconv.Itoa(order)), nil)
			order++
		}
	}
	for _, n := range devs.Children("interface") {
		if b := n.Child("boot"); b != nil {
			b.SetAttr("order", strconv.Itoa(order))
			order++
		}
	}
	for _, h := range devs.Children("hostdev") {
		if b := h.Child("boot"); b != nil {
			b.SetAttr("order", strconv.Itoa(order))
			order++
		}
	}
}

func applyDisplay(devs *Node, cfg VMConfig) error {
	if !slices.Contains(videos, cfg.Video) && cfg.Video != devs.Path("video", "model").Attr("type") {
		return fmt.Errorf("unknown display adapter %q", cfg.Video)
	}
	var vnc *Node
	for _, g := range devs.Children("graphics") {
		if g.Attr("type") == "vnc" {
			if vnc != nil || !cfg.VNC {
				devs.Remove(g)
				continue
			}
			vnc = g
		}
	}
	if cfg.VNC && vnc == nil {
		devs.Add(El("graphics", "type", "vnc", "port", "-1", "autoport", "yes", "listen", "127.0.0.1").
			With(El("listen", "type", "address", "address", "127.0.0.1")), nil)
	}
	video := devs.Child("video")
	if video == nil {
		if cfg.Video == "none" {
			return nil // it has none, and none is wanted
		}
		video = devs.Add(El("video"), nil)
	}
	m := video.Child("model")
	if m == nil {
		m = video.Add(El("model"), nil)
	}
	if m.Attr("type") != cfg.Video {
		*m = *El("model", "type", cfg.Video)
		if cfg.Video != "none" {
			m.SetAttr("heads", "1")
			m.SetAttr("primary", "yes")
		}
		if a := video.Child("address"); a != nil {
			video.Remove(a)
		}
	}
	return nil
}

func applyUSB(devs *Node, want []USBDevice) error {
	key := func(v, p string) string { return strings.ToLower(v + ":" + p) }
	wanted := map[string]bool{}
	for _, u := range want {
		if !reUSBID.MatchString(u.Vendor) || !reUSBID.MatchString(u.Product) {
			return fmt.Errorf("bad USB device id %s:%s", u.Vendor, u.Product)
		}
		wanted[key(u.Vendor, u.Product)] = true
	}
	have := map[string]bool{}
	for _, h := range devs.Children("hostdev") {
		src := h.Child("source")
		if h.Attr("type") != "usb" || src.Child("vendor") == nil {
			continue
		}
		k := key(src.Child("vendor").Attr("id"), src.Child("product").Attr("id"))
		if !wanted[k] || have[k] {
			devs.Remove(h)
			continue
		}
		have[k] = true
	}
	for _, u := range want {
		if k := key(u.Vendor, u.Product); !have[k] {
			have[k] = true
			devs.Add(El("hostdev", "mode", "subsystem", "type", "usb", "managed", "yes").
				With(El("source").With(El("vendor", "id", strings.ToLower(u.Vendor)), El("product", "id", strings.ToLower(u.Product)))), nil)
		}
	}
	return nil
}

// ---------- Through libvirt ----------

// Config returns a VM's settings for the form, with disk sizes.
func (c *Client) Config(ctx context.Context, name string) (VMConfig, error) {
	x, err := c.DumpXML(ctx, name, true)
	if err != nil {
		return VMConfig{}, err
	}
	cfg, err := ReadConfig(x)
	if err != nil {
		return VMConfig{}, err
	}
	for i, d := range cfg.Disks {
		if d.Device == "disk" && !d.Fixed && d.Source != "" {
			cfg.Disks[i].Size = c.diskCapacity(ctx, name, d.Target)
		}
	}
	return cfg, nil
}

func (c *Client) diskCapacity(ctx context.Context, vm, target string) int64 {
	out, err := c.run(ctx, "domblkinfo", "--", vm, target)
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Capacity:"); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// SaveConfig applies the form: creates new disks (in domainsDir/<name>/),
// saves the definition, then grows any disks that were made bigger.
func (c *Client) SaveConfig(ctx context.Context, name string, cfg VMConfig, domainsDir string) error {
	x, err := c.DumpXML(ctx, name, true)
	if err != nil {
		return err
	}
	cur, err := ReadConfig(x)
	if err != nil {
		return err
	}
	vm, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	off := vm.State == "shutoff" || vm.State == "crashed"

	// Paths for new disks: vdisk2.qcow2, vdisk3… next to the VM's others.
	taken := map[string]bool{}
	for _, d := range cur.Disks {
		taken[d.Source] = true
	}
	folder := filepath.Join(strings.TrimRight(firstNonEmpty(domainsDir, "/mnt/user/domains"), "/"), name)
	var create []DiskConfig
	for i, d := range cfg.Disks {
		if d.Target == "" && !d.Remove && d.Device == "disk" && d.Source == "" {
			if d.NewSizeGB < 1 {
				return errors.New("give each new disk a size of at least 1 GB")
			}
			for n := 1; ; n++ {
				p := filepath.Join(folder, fmt.Sprintf("vdisk%d.qcow2", n))
				if !taken[p] && !c.FileExists(ctx, p) {
					taken[p] = true
					cfg.Disks[i].Source = p
					create = append(create, cfg.Disks[i])
					break
				}
			}
		}
	}
	// Disks to grow (never shrink: that would cut the guest's filesystem).
	sizes := map[string]int64{}
	for _, d := range cur.Disks {
		sizes[d.Target] = c.diskCapacity(ctx, name, d.Target)
	}
	var grow []DiskConfig
	for _, d := range cfg.Disks {
		if d.Target != "" && !d.Remove && d.Device == "disk" && d.NewSizeGB > 0 {
			want := int64(d.NewSizeGB) << 30
			if want < sizes[d.Target] {
				return fmt.Errorf("%s can only be made bigger (shrinking would damage the data on it)", d.Target)
			}
			if want > sizes[d.Target] {
				if !off {
					return fmt.Errorf("shut the VM down before making %s bigger", d.Target)
				}
				grow = append(grow, d)
			}
		}
	}

	out, err := ApplyConfig(x, cfg)
	if err != nil {
		return err
	}
	var made []string
	for _, d := range create {
		if err := c.CreateDisk(ctx, d.Source, d.NewSizeGB); err != nil {
			for _, p := range made {
				_ = c.DeleteFile(context.Background(), p)
			}
			return fmt.Errorf("couldn't create %s: %v", d.Source, err)
		}
		made = append(made, d.Source)
	}
	if err := c.Define(ctx, out); err != nil {
		for _, p := range made {
			_ = c.DeleteFile(context.Background(), p)
		}
		return err
	}
	for _, d := range grow {
		src := d.Source
		for _, cd := range cur.Disks {
			if cd.Target == d.Target {
				src = firstNonEmpty(src, cd.Source)
			}
		}
		if err := c.ResizeDisk(ctx, src, d.NewSizeGB); err != nil {
			return fmt.Errorf("the settings were saved, but %s couldn't be made bigger: %v", d.Target, err)
		}
	}
	return nil
}

// ResizeDisk grows a disk image to sizeGiB.
func (c *Client) ResizeDisk(ctx context.Context, path string, sizeGiB int) error {
	dir, file := splitPath(path)
	return c.withDirPool(ctx, dir, func(pool string) error {
		_, err := c.run(ctx, "vol-resize", "--pool", pool, file, fmt.Sprintf("%dG", sizeGiB))
		return err
	})
}

// HostUSB lists the host's USB devices that can be passed to a VM (hubs
// left out).
func (c *Client) HostUSB(ctx context.Context) []USBDevice {
	out, err := c.run(ctx, "nodedev-list", "--cap", "usb_device")
	res := []USBDevice{}
	if err != nil {
		return res
	}
	for name := range strings.SplitSeq(out, "\n") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		x, err := c.run(ctx, "nodedev-dumpxml", "--", name)
		if err != nil {
			continue
		}
		var d struct {
			Cap struct {
				Product struct {
					ID   string `xml:"id,attr"`
					Name string `xml:",chardata"`
				} `xml:"product"`
				Vendor struct {
					ID   string `xml:"id,attr"`
					Name string `xml:",chardata"`
				} `xml:"vendor"`
			} `xml:"capability"`
		}
		if xml.Unmarshal([]byte(x), &d) != nil || d.Cap.Vendor.ID == "0x1d6b" { // Linux Foundation: root hubs
			continue
		}
		if strings.Contains(strings.ToLower(d.Cap.Product.Name), "hub") {
			continue
		}
		name := strings.TrimSpace(strings.TrimSpace(d.Cap.Vendor.Name) + " " + strings.TrimSpace(d.Cap.Product.Name))
		res = append(res, USBDevice{Vendor: d.Cap.Vendor.ID, Product: d.Cap.Product.ID, Name: name})
	}
	return res
}

// applySerial adds a serial port (with the console on it) or removes it.
// Only the ordinary kind ("pty", which `virsh console` uses) is removed;
// any other kind someone set up by hand stays.
func applySerial(devs *Node, want bool) {
	has := devs.Child("serial") != nil || devs.Child("console") != nil
	switch {
	case want && !has:
		devs.Add(El("serial", "type", "pty").With(El("target", "port", "0")), nil)
		devs.Add(El("console", "type", "pty").With(El("target", "type", "serial", "port", "0")), nil)
	case !want && has:
		for _, kind := range []string{"serial", "console"} {
			for _, n := range devs.Children(kind) {
				if n.Attr("type") == "pty" {
					devs.Remove(n)
				}
			}
		}
	}
}
