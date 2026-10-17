package libvirt

import (
	"encoding/xml"
	"strings"
	"testing"
)

// An Unraid-made VM, as `virsh dumpxml --inactive` shows it.
const unraidVM = `<domain type='kvm' id='1'>
  <name>Windows 11</name>
  <uuid>8b0f4c7e-1111-2222-3333-444455556666</uuid>
  <metadata>
    <vmtemplate xmlns="unraid" name="Windows 11" icon="windows11.png" os="windowstpm"/>
    <homestack:vm xmlns:homestack="https://homestack.local/xmlns/vm" os="windows"/>
  </metadata>
  <memory unit='KiB'>8388608</memory>
  <currentMemory unit='KiB'>8388608</currentMemory>
  <memoryBacking>
    <nosharepages/>
  </memoryBacking>
  <vcpu placement='static'>4</vcpu>
  <cputune>
    <vcpupin vcpu='0' cpuset='2'/>
    <vcpupin vcpu='1' cpuset='10'/>
    <vcpupin vcpu='2' cpuset='3'/>
    <vcpupin vcpu='3' cpuset='11'/>
  </cputune>
  <os>
    <type arch='x86_64' machine='pc-q35-9.2'>hvm</type>
    <loader readonly='yes' type='pflash'>/usr/share/qemu/ovmf-x64/OVMF_CODE-pure-efi-tpm.fd</loader>
    <nvram>/etc/libvirt/qemu/nvram/8b0f4c7e_VARS-pure-efi-tpm.fd</nvram>
  </os>
  <cpu mode='host-passthrough' check='none' migratable='on'>
    <topology sockets='1' dies='1' clusters='1' cores='2' threads='2'/>
    <cache mode='passthrough'/>
  </cpu>
  <devices>
    <!-- keep me -->
    <emulator>/usr/local/sbin/qemu</emulator>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw' cache='writeback'/>
      <source file='/mnt/user/domains/Windows 11/vdisk1.img'/>
      <target dev='hdc' bus='sata'/>
      <boot order='1'/>
      <address type='drive' controller='0' bus='0' target='0' unit='2'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/mnt/user/isos/Win11_24H2.iso'/>
      <target dev='hda' bus='sata'/>
      <readonly/>
      <boot order='2'/>
    </disk>
    <interface type='bridge'>
      <mac address='52:54:00:aa:bb:cc'/>
      <source bridge='br0'/>
      <model type='virtio-net'/>
    </interface>
    <graphics type='vnc' port='-1' autoport='yes' websocket='-1' listen='0.0.0.0' keymap='en-us'>
      <listen type='address' address='0.0.0.0'/>
    </graphics>
    <video>
      <model type='qxl' ram='65536' vram='65536' vgamem='16384' heads='1' primary='yes'/>
    </video>
    <hostdev mode='subsystem' type='pci' managed='yes'>
      <source>
        <address domain='0x0000' bus='0x0b' slot='0x00' function='0x0'/>
      </source>
    </hostdev>
    <hostdev mode='subsystem' type='usb' managed='no'>
      <source>
        <vendor id='0x10c4'/>
        <product id='0xea60'/>
      </source>
    </hostdev>
  </devices>
</domain>
`

func mustParse(t *testing.T, x string) DomainXML {
	t.Helper()
	var d DomainXML
	if err := xml.Unmarshal([]byte(x), &d); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, x)
	}
	return d
}

func TestReadConfig(t *testing.T) {
	cfg, err := ReadConfig(unraidVM)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "Windows 11" || cfg.VCPUs != 4 || cfg.MemoryMB != 8192 || cfg.Firmware != "uefi" || cfg.Boot != "disk" || !cfg.VNC || cfg.Video != "qxl" {
		t.Errorf("basics %+v", cfg)
	}
	if len(cfg.Disks) != 2 || cfg.Disks[0].Target != "hdc" || cfg.Disks[1].Device != "cdrom" || cfg.Disks[1].Source != "/mnt/user/isos/Win11_24H2.iso" {
		t.Errorf("disks %+v", cfg.Disks)
	}
	if len(cfg.NICs) != 1 || cfg.NICs[0].Source != "br0" || cfg.NICs[0].Kind != "bridge" {
		t.Errorf("nics %+v", cfg.NICs)
	}
	if len(cfg.USB) != 1 || cfg.USB[0].Vendor != "0x10c4" {
		t.Errorf("usb %+v", cfg.USB)
	}
	if strings.Join(cfg.Kept, "|") != "1 PCI device passed through (a graphics card, controller…)|CPU pinning (4 vCPUs pinned to host threads)" {
		t.Errorf("kept %q", cfg.Kept)
	}
}

func TestApplyConfigUnchangedKeepsEverything(t *testing.T) {
	cfg, _ := ReadConfig(unraidVM)
	out, err := ApplyConfig(unraidVM, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustParse(t, out)
	for _, want := range []string{
		`<vmtemplate xmlns='unraid' name='Windows 11' icon='windows11.png' os='windowstpm'/>`,
		`<homestack:vm xmlns:homestack='https://homestack.local/xmlns/vm' os='windows'/>`,
		`<!-- keep me -->`, `<nosharepages/>`, `<vcpupin vcpu='3' cpuset='11'/>`, `<cache mode='passthrough'/>`,
		`<address domain='0x0000' bus='0x0b' slot='0x00' function='0x0'/>`, `websocket='-1'`, `vram='65536'`,
		`<nvram>/etc/libvirt/qemu/nvram/8b0f4c7e_VARS-pure-efi-tpm.fd</nvram>`, `<model type='virtio-net'/>`,
		`<address type='drive' controller='0' bus='0' target='0' unit='2'/>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %s", want)
		}
	}
	if again, _ := ReadConfig(out); again.VCPUs != 4 || again.MemoryMB != 8192 || len(again.Disks) != 2 {
		t.Errorf("round trip %+v", again)
	}
}

func TestApplyConfigChanges(t *testing.T) {
	cfg, _ := ReadConfig(unraidVM)
	cfg.Description = "Gaming & office"
	cfg.VCPUs = 2
	cfg.MemoryMB = 12288
	cfg.Disks[0].Bus = "virtio"                                                                                                 // SATA -> virtio
	cfg.Disks[1].Source = ""                                                                                                    // eject the CD
	cfg.Disks = append(cfg.Disks, DiskConfig{Device: "disk", Bus: "scsi", Source: "/mnt/user/domains/Windows 11/vdisk2.qcow2"}) // new
	cfg.NICs[0].Source = "br0.40"
	cfg.NICs = append(cfg.NICs, NICConfig{Kind: "network", Source: "default", Model: "e1000e"})
	cfg.Boot = "cdrom"
	cfg.VNC = false
	cfg.Video = "virtio"
	cfg.USB = []USBDevice{{Vendor: "0x1a86", Product: "0x55d4"}}

	out, err := ApplyConfig(unraidVM, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := mustParse(t, out)
	got, _ := ReadConfig(out)
	if got.Description != "Gaming & office" || got.VCPUs != 2 || got.MemoryMB != 12288 || got.Boot != "cdrom" || got.VNC || got.Video != "virtio" {
		t.Errorf("basics %+v", got)
	}
	if !strings.Contains(out, "cores='1' threads='2'") {
		t.Error("topology not matched to 2 vCPUs")
	}
	if strings.Contains(out, "vcpu='2'") || !strings.Contains(out, "vcpu='1'") {
		t.Error("pins for removed vCPUs kept, or the rest dropped")
	}
	disks := d.Devices.Disks
	if len(disks) != 3 || disks[0].Target.Dev != "vda" || disks[0].Target.Bus != "virtio" || strings.Contains(out, "type='drive'") {
		t.Errorf("bus change: %+v", disks)
	}
	if disks[1].Source.File != "" {
		t.Error("CD not ejected")
	}
	if disks[2].Target.Bus != "scsi" || disks[2].Target.Dev != "sda" || disks[2].Driver.Type != "qcow2" || !strings.Contains(out, "model='virtio-scsi'") {
		t.Errorf("new disk: %+v", disks[2])
	}
	if !strings.Contains(out, "<boot order='1'/>") || strings.Count(out, "<boot order=") != 2 {
		t.Error("boot order")
	}
	if len(d.Devices.Interfaces) != 2 || d.Devices.Interfaces[0].Source.Bridge != "br0.40" || d.Devices.Interfaces[0].MAC.Address != "52:54:00:aa:bb:cc" || d.Devices.Interfaces[1].Source.Network != "default" {
		t.Errorf("nics %+v", d.Devices.Interfaces)
	}
	if len(d.Devices.Graphics) != 0 || strings.Contains(out, "vram=") {
		t.Error("display")
	}
	if !strings.Contains(out, "<vendor id='0x1a86'/>") || strings.Contains(out, "0x10c4") || !strings.Contains(out, "type='pci'") {
		t.Error("usb devices")
	}
	if !strings.Contains(out, "Gaming &amp; office") {
		t.Error("description not escaped")
	}

	// Removing a disk and a card, and turning VNC back on.
	got.Disks[2].Remove = true
	got.NICs[1].Remove = true
	got.VNC = true
	out2, err := ApplyConfig(out, got)
	if err != nil {
		t.Fatal(err)
	}
	d2 := mustParse(t, out2)
	if len(d2.Devices.Disks) != 2 || len(d2.Devices.Interfaces) != 1 || len(d2.Devices.Graphics) != 1 || d2.Devices.Graphics[0].Listen != "127.0.0.1" {
		t.Errorf("removals: %d disks, %d nics, %+v", len(d2.Devices.Disks), len(d2.Devices.Interfaces), d2.Devices.Graphics)
	}
}

func TestApplyConfigRefuses(t *testing.T) {
	base, _ := ReadConfig(unraidVM)
	bad := []func(c *VMConfig){
		func(c *VMConfig) { c.Name = "Other" },
		func(c *VMConfig) { c.VCPUs = 0 },
		func(c *VMConfig) { c.MemoryMB = 64 },
		func(c *VMConfig) { c.Disks[1].Bus = "virtio" }, // CDs can't be virtio
		func(c *VMConfig) { c.Disks[0].Source = "relative.img" },
		func(c *VMConfig) { c.Disks[0].Target = "vdz" },
		func(c *VMConfig) { c.NICs[0].Model = "nope" },
		func(c *VMConfig) { c.NICs[0].Source = "br0' evil='1" },
		func(c *VMConfig) { c.Video = "3dfx" },
		func(c *VMConfig) { c.USB = []USBDevice{{Vendor: "x", Product: "y"}} },
	}
	for i, f := range bad {
		cfg, _ := ReadConfig(unraidVM)
		cfg.Disks = append([]DiskConfig(nil), base.Disks...)
		cfg.NICs = append([]NICConfig(nil), base.NICs...)
		f(&cfg)
		if _, err := ApplyConfig(unraidVM, cfg); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestDomRoundTrip(t *testing.T) {
	in := "<a x='1 &amp; 2' y=\"it's\"><!-- c --><b>t &lt; u</b><c/>\n</a>"
	n, err := ParseDoc(in)
	if err != nil {
		t.Fatal(err)
	}
	out := n.String()
	if out != "<a x='1 &amp; 2' y='it&apos;s'><!-- c --><b>t &lt; u</b><c/>\n</a>\n" {
		t.Fatalf("got %q", out)
	}
	if _, err := ParseDoc("<a><b></a>"); err == nil {
		t.Fatal("mismatched tags accepted")
	}
}

func TestApplySerial(t *testing.T) {
	cfg, _ := ReadConfig(unraidVM)
	if cfg.Serial == nil || *cfg.Serial {
		t.Fatalf("the Unraid VM has no serial port: %v", cfg.Serial)
	}
	// A save that doesn't mention it (an older page) leaves it alone.
	cfg.Serial = nil
	if out, _ := ApplyConfig(unraidVM, cfg); strings.Contains(out, "<serial") {
		t.Error("serial added without being asked")
	}
	on := true
	cfg.Serial = &on
	out, err := ApplyConfig(unraidVM, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustParse(t, out)
	if !strings.Contains(out, "<serial type='pty'><target port='0'/></serial>") || !strings.Contains(out, "<console type='pty'><target type='serial' port='0'/></console>") {
		t.Errorf("serial port missing:\n%s", out)
	}
	if again, _ := ReadConfig(out); again.Serial == nil || !*again.Serial {
		t.Error("read back without the serial port")
	}
	// Taking it away removes the pty kind, but never a hand-made one.
	off := false
	cfg.Serial = &off
	out2, _ := ApplyConfig(out, cfg)
	if strings.Contains(out2, "<serial") || strings.Contains(out2, "<console") {
		t.Errorf("serial port still there:\n%s", out2)
	}
	tcp := strings.Replace(out, "<serial type='pty'>", "<serial type='tcp'>", 1)
	if out3, _ := ApplyConfig(tcp, cfg); !strings.Contains(out3, "<serial type='tcp'>") {
		t.Error("a hand-made serial port was removed")
	}
}
