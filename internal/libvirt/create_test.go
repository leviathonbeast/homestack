package libvirt

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestBuildXML(t *testing.T) {
	caps := Caps{DomainType: "kvm", Emulator: "/usr/bin/qemu-system-x86_64", Machine: "q35", Loaders: []string{"/usr/share/OVMF/OVMF_CODE_4M.fd"}, TPM: true}
	v := NewVM{Name: "win11", OS: "windows", VCPUs: 4, MemoryMB: 8192, Firmware: "uefi", ISO: "/mnt/user/isos/Win11 & more.iso", TPM: true}
	v.Disk.Mode, v.Disk.Path = "existing", "/mnt/user/domains/win11/vdisk1.qcow2"
	v.Network.Kind, v.Network.Source = "bridge", "br0.40"
	v.USB = []USBDevice{{Vendor: "0x10c4", Product: "0xea60"}}
	out, err := BuildXML(v, caps)
	if err != nil {
		t.Fatal(err)
	}
	var d DomainXML
	if err := xml.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, out)
	}
	vm := summarize(d)
	if vm.Firmware != "uefi" || vm.OS != "windows" || vm.VCPUs != 4 || vm.Memory != 8192<<20 {
		t.Errorf("unexpected summary %+v", vm)
	}
	for _, want := range []string{"bus='sata'", "e1000e", "<tpm model='tpm-crb'>", "host-passthrough", "localtime", "Win11 &amp; more.iso", "<vendor id='0x10c4'/>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if _, err := BuildXML(NewVM{Name: "bad name!", VCPUs: 1, MemoryMB: 512}, caps); err == nil {
		t.Error("bad name accepted")
	}
	if _, err := BuildXML(NewVM{Name: "ok", VCPUs: 1, MemoryMB: 512, Firmware: "bios", USB: []USBDevice{{Vendor: "0x1'/>", Product: "0x1"}}}, caps); err == nil {
		t.Error("bad USB id accepted")
	}
	// What the form builds, the settings form can read back.
	if cfg, err := ReadConfig(out); err != nil || len(cfg.USB) != 1 || cfg.Boot != "disk" || cfg.VCPUs != 4 {
		t.Errorf("ReadConfig of a new VM: %v %+v", err, cfg)
	}
}

func TestToBytes(t *testing.T) {
	if toBytes(2097152, "KiB") != 2<<30 || toBytes(4, "GiB") != 4<<30 {
		t.Fatal("unit conversion wrong")
	}
}
