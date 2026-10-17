package libvirt

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// NewVM is what the "New VM" form sends.
type NewVM struct {
	Name     string `json:"name"`
	OS       string `json:"os"` // linux | windows | other
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memoryMB"`
	Firmware string `json:"firmware"` // uefi | bios
	Disk     struct {
		Mode   string `json:"mode"` // new | existing | none
		SizeGB int    `json:"sizeGB"`
		Path   string `json:"path"`
		Folder string `json:"folder"` // where new disks go, e.g. /mnt/user/domains
		Virtio bool   `json:"virtio"`
	} `json:"disk"`
	ISO       string `json:"iso"`
	VirtioISO string `json:"virtioIso"` // Windows driver ISO, optional
	Network   struct {
		Kind   string `json:"kind"`          // bridge | network
		Source string `json:"source"`        // br0, br0.40, default...
		MAC    string `json:"mac,omitempty"` // fixed, so cloud-init can match the card
	} `json:"network"`
	TPM   bool        `json:"tpm"`
	Start bool        `json:"start"`
	USB   []USBDevice `json:"usb"` // host USB devices to pass through

	// From a template (see templates.go)
	Template string `json:"template,omitempty"` // its id
	Clone    string `json:"clone,omitempty"`    // linked | full
}

var vmNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), "'", "&#39;")
}

var domainTmpl = template.Must(template.New("vm").Funcs(template.FuncMap{"x": esc}).Parse(`<domain type='{{x .Caps.DomainType}}'>
  <name>{{x .Name}}</name>
  <metadata>
    <homestack:vm xmlns:homestack="https://homestack.local/xmlns/vm" os="{{x .OS}}"{{if .Template}} template="{{x .Template}}"{{end}}/>
  </metadata>
  <memory unit='MiB'>{{.MemoryMB}}</memory>
  <currentMemory unit='MiB'>{{.MemoryMB}}</currentMemory>
  <vcpu placement='static'>{{.VCPUs}}</vcpu>
  <os>
    <type arch='x86_64' machine='{{x .Caps.Machine}}'>hvm</type>
{{- if .Loader}}
    <loader readonly='yes' type='pflash'>{{x .Loader}}</loader>
{{- end}}
  </os>
  <features>
    <acpi/>
    <apic/>
{{- if .Windows}}
    <hyperv mode='custom'>
      <relaxed state='on'/>
      <vapic state='on'/>
      <spinlocks state='on' retries='8191'/>
    </hyperv>
{{- end}}
  </features>
{{- if eq .Caps.DomainType "kvm"}}
  <cpu mode='host-passthrough' check='none' migratable='on'/>
{{- end}}
  <clock offset='{{if .Windows}}localtime{{else}}utc{{end}}'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
{{- if .Windows}}
    <timer name='hypervclock' present='yes'/>
{{- end}}
  </clock>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>restart</on_crash>
  <devices>
    <emulator>{{x .Caps.Emulator}}</emulator>
{{- if .DiskPath}}
    <disk type='file' device='disk'>
      <driver name='qemu' type='{{.DiskFormat}}' cache='writeback' discard='unmap'/>
      <source file='{{x .DiskPath}}'/>
      <target dev='{{.DiskDev}}' bus='{{.DiskBus}}'/>
      <boot order='1'/>
    </disk>
{{- end}}
{{- if .ISO}}
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='{{x .ISO}}'/>
      <target dev='sdb' bus='sata'/>
      <readonly/>
      <boot order='2'/>
    </disk>
{{- end}}
{{- if .VirtioISO}}
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='{{x .VirtioISO}}'/>
      <target dev='sdc' bus='sata'/>
      <readonly/>
    </disk>
{{- end}}
    <controller type='usb' model='qemu-xhci' ports='15'/>
{{- if .NetSource}}
    <interface type='{{.NetKind}}'>
{{- if .Network.MAC}}
      <mac address='{{x .Network.MAC}}'/>
{{- end}}
      <source {{.NetKind}}='{{x .NetSource}}'/>
      <model type='{{.NICModel}}'/>
    </interface>
{{- end}}
    <input type='tablet' bus='usb'/>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <video>
      <model type='{{if .Windows}}vga{{else}}virtio{{end}}' heads='1' primary='yes'/>
    </video>
    <serial type='pty'>
      <target port='0'/>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
{{- if .TPM}}
    <tpm model='tpm-crb'>
      <backend type='emulator' version='2.0'/>
    </tpm>
{{- end}}
{{- range .USB}}
    <hostdev mode='subsystem' type='usb' managed='yes'>
      <source>
        <vendor id='{{x .Vendor}}'/>
        <product id='{{x .Product}}'/>
      </source>
    </hostdev>
{{- end}}
    <memballoon model='virtio'>
      <stats period='10'/>
    </memballoon>
    <rng model='virtio'>
      <backend model='random'>/dev/urandom</backend>
    </rng>
  </devices>
</domain>
`))

type tmplData struct {
	NewVM
	Caps       Caps
	Windows    bool
	Loader     string
	DiskPath   string
	DiskFormat string
	DiskDev    string
	DiskBus    string
	NetKind    string
	NetSource  string
	NICModel   string
}

// BuildXML turns the form into libvirt domain XML.
func BuildXML(v NewVM, caps Caps) (string, error) {
	if !vmNameRe.MatchString(v.Name) {
		return "", errors.New("use letters, numbers, dots, dashes and underscores for the name")
	}
	if v.VCPUs < 1 || v.VCPUs > 128 {
		return "", errors.New("pick between 1 and 128 vCPUs")
	}
	if v.MemoryMB < 256 {
		return "", errors.New("give the VM at least 256 MB of memory")
	}
	d := tmplData{NewVM: v, Caps: caps, Windows: v.OS == "windows"}
	if v.Firmware != "bios" {
		if len(caps.Loaders) == 0 {
			return "", errors.New("no UEFI firmware (OVMF) was found on this host. Install it, or choose BIOS")
		}
		d.Loader = caps.Loaders[0]
	}
	if v.TPM && !caps.TPM {
		return "", errors.New("this host can't emulate a TPM (install swtpm), so untick TPM")
	}
	for _, u := range v.USB {
		if !reUSBID.MatchString(u.Vendor) || !reUSBID.MatchString(u.Product) {
			return "", fmt.Errorf("bad USB device id %s:%s", u.Vendor, u.Product)
		}
	}
	switch v.Disk.Mode {
	case "new", "existing":
		d.DiskPath = v.Disk.Path
		d.DiskFormat = "qcow2"
		if strings.HasSuffix(strings.ToLower(v.Disk.Path), ".img") || strings.HasSuffix(strings.ToLower(v.Disk.Path), ".raw") {
			d.DiskFormat = "raw"
		}
		// Windows has no virtio drivers until you install them, so it gets SATA
		// unless asked otherwise.
		if v.Disk.Virtio || !d.Windows {
			d.DiskDev, d.DiskBus = "vda", "virtio"
		} else {
			d.DiskDev, d.DiskBus = "sda", "sata"
		}
	case "none", "":
	default:
		return "", errors.New("unknown disk option")
	}
	if v.Network.MAC != "" && !reMAC.MatchString(v.Network.MAC) {
		return "", errors.New("bad MAC address")
	}
	if src := strings.TrimSpace(v.Network.Source); src != "" {
		d.NetSource = src
		d.NetKind = "bridge"
		if v.Network.Kind == "network" {
			d.NetKind = "network"
		}
		d.NICModel = "virtio"
		if d.Windows {
			d.NICModel = "e1000e"
		}
	}
	var b bytes.Buffer
	if err := domainTmpl.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Create builds the disk (if asked) and defines the VM.
func (c *Client) Create(ctx context.Context, v NewVM) (string, error) {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return "", err
	}
	if v.Disk.Mode == "new" {
		if v.Disk.SizeGB < 1 {
			return "", errors.New("give the disk a size of at least 1 GB")
		}
		folder := strings.TrimRight(firstNonEmpty(v.Disk.Folder, "/mnt/user/domains"), "/")
		if !filepath.IsAbs(folder) {
			return "", errors.New("use a full path for the VM folder")
		}
		v.Disk.Path = filepath.Join(folder, v.Name, "vdisk1.qcow2")
	}
	xmlText, err := BuildXML(v, caps)
	if err != nil {
		return "", err
	}
	if _, err := c.run(ctx, "dominfo", "--", v.Name); err == nil {
		return "", fmt.Errorf("a VM called %s already exists", v.Name)
	}
	createdDisk := false
	if v.Disk.Mode == "new" {
		if err := c.CreateDisk(ctx, v.Disk.Path, v.Disk.SizeGB); err != nil {
			return "", fmt.Errorf("couldn't create the disk: %v", err)
		}
		createdDisk = true
	}
	if err := c.Define(ctx, xmlText); err != nil {
		if createdDisk {
			_ = c.DeleteFile(context.Background(), v.Disk.Path)
		}
		return "", err
	}
	if v.Start {
		if err := c.Action(ctx, v.Name, "start"); err != nil {
			return xmlText, fmt.Errorf("the VM was created but didn't start: %v", err)
		}
	}
	return xmlText, nil
}
