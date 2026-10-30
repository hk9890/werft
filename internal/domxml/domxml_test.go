package domxml_test

import (
	"reflect"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
)

const uefiDomain = `<domain type='kvm'>
  <name>ubuntu-26-04</name>
  <description>first line
vm: the disk asks for the guest password at boot</description>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <loader readonly='yes' type='pflash' format='raw'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
    <nvram template='/usr/share/OVMF/OVMF_VARS_4M.qcow2' templateFormat='qcow2' format='qcow2'>/var/lib/libvirt/qemu/nvram/ubuntu-26-04_VARS.qcow2</nvram>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/ubuntu-26-04.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='volume' device='disk'>
      <driver name='qemu' type='raw'/>
      <source pool='werft' volume='data.raw'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='block' device='cdrom'>
      <source dev='/dev/sr0'/>
      <target dev='sda' bus='sata'/>
    </disk>
    <interface type='network'><mac address='52:54:00:aa:bb:cc'/><source network='default'/></interface>
    <interface type='network'><source network='default'/></interface>
    <vsock model='virtio'><cid auto='yes' address='42'/></vsock>
  </devices>
</domain>`

const biosDomain = `<domain type='kvm'>
  <name>bios</name>
  <os><type arch='x86_64' machine='pc'>hvm</type></os>
  <devices><vsock model='virtio'><cid auto='yes'/></vsock></devices>
</domain>`

func TestUEFIDomain(t *testing.T) {
	d, err := domxml.Parse(uefiDomain)
	if err != nil {
		t.Fatal(err)
	}
	wantDisks := []domxml.Disk{
		{Target: "vda", Device: "disk", Format: "qcow2", Source: "/var/lib/libvirt/images/ubuntu-26-04.qcow2"},
		{Target: "vdb", Device: "disk", Format: "raw", Pool: "werft", Volume: "data.raw"},
		{Target: "sda", Device: "cdrom", Source: "/dev/sr0"},
	}
	if got := domxml.Disks(d); !reflect.DeepEqual(got, wantDisks) {
		t.Errorf("Disks = %+v", got)
	}
	loader, ok := domxml.LoaderOf(d)
	if !ok || loader != (domxml.Loader{Path: "/usr/share/OVMF/OVMF_CODE_4M.fd", Type: "pflash", Format: "raw", Readonly: true}) {
		t.Errorf("LoaderOf = %+v, %v", loader, ok)
	}
	nvram, ok := domxml.NVRAMOf(d)
	wantNVRAM := domxml.NVRAM{
		Path:           "/var/lib/libvirt/qemu/nvram/ubuntu-26-04_VARS.qcow2",
		Format:         "qcow2",
		Template:       "/usr/share/OVMF/OVMF_VARS_4M.qcow2",
		TemplateFormat: "qcow2",
	}
	if !ok || nvram != wantNVRAM {
		t.Errorf("NVRAMOf = %+v, %v", nvram, ok)
	}
	if !domxml.UEFI(d) {
		t.Error("UEFI = false")
	}
	if cid, ok := domxml.VsockCID(d); !ok || cid != 42 {
		t.Errorf("VsockCID = %d, %v", cid, ok)
	}
	if got := domxml.Description(d); got != "first line\nvm: the disk asks for the guest password at boot" {
		t.Errorf("Description = %q", got)
	}
}

func TestBIOSDomain(t *testing.T) {
	d, err := domxml.Parse(biosDomain)
	if err != nil {
		t.Fatal(err)
	}
	if domxml.UEFI(d) {
		t.Error("UEFI = true for a BIOS domain")
	}
	if _, ok := domxml.LoaderOf(d); ok {
		t.Error("LoaderOf found a loader")
	}
	if _, ok := domxml.NVRAMOf(d); ok {
		t.Error("NVRAMOf found an NVRAM")
	}
	if _, ok := domxml.VsockCID(d); ok {
		t.Error("VsockCID found a CID in an inactive auto vsock")
	}
	if domxml.Disks(d) != nil || domxml.Interfaces(d) != nil {
		t.Error("found disks or interfaces in a domain without them")
	}
}

func TestFirmwareAutoselectionIsUEFI(t *testing.T) {
	d, err := domxml.Parse(`<domain type='kvm'><name>x</name><os firmware='efi'><type>hvm</type></os></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	if !domxml.UEFI(d) {
		t.Error("UEFI = false for firmware='efi'")
	}
}

func TestNVRAMFromSourceElement(t *testing.T) {
	d, err := domxml.Parse(`<domain type='kvm'><name>x</name><os><type>hvm</type>
<nvram type='file' format='qcow2'><source file='/nvram/x.qcow2'/></nvram></os></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := domxml.NVRAMOf(d); n.Path != "/nvram/x.qcow2" {
		t.Errorf("NVRAMOf path = %q", n.Path)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := domxml.Parse("<domain"); err == nil {
		t.Error("Parse accepted broken XML")
	}
}
