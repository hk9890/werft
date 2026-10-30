package domxml_test

import (
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
)

func TestClone(t *testing.T) {
	src, err := domxml.Parse(uefiDomain)
	if err != nil {
		t.Fatal(err)
	}
	src.UUID = "0b5a1c7e-0000-4000-8000-000000000001"
	c, err := domxml.Clone(src, "ubuntu-26-04-full", map[string]string{"vda": "/pool/ubuntu-26-04-full.qcow2", "vdb": "/pool/ubuntu-26-04-full-1.raw"}, "/nvram/ubuntu-26-04-full_VARS.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "ubuntu-26-04-full" || c.UUID != "" {
		t.Errorf("name %q, uuid %q", c.Name, c.UUID)
	}
	disks := domxml.Disks(c)
	if disks[0].Source != "/pool/ubuntu-26-04-full.qcow2" || disks[1].Source != "/pool/ubuntu-26-04-full-1.raw" || disks[1].Pool != "" {
		t.Errorf("disk sources not replaced: %+v", disks)
	}
	if disks[2].Source != "/dev/sr0" {
		t.Errorf("a disk without a copy lost its source: %+v", disks[2])
	}
	nv, _ := domxml.NVRAMOf(c)
	want := domxml.NVRAM{Path: "/nvram/ubuntu-26-04-full_VARS.qcow2", Format: "qcow2"}
	if nv != want {
		t.Errorf("nvram = %+v, want %+v", nv, want)
	}
	ifaces := domxml.Interfaces(c)
	if mac := ifaces[0].MAC; mac == "52:54:00:aa:bb:cc" || !strings.HasPrefix(mac, "52:54:00:") || ifaces[1].MAC != "" {
		t.Errorf("Interfaces = %+v", ifaces)
	}
	if _, ok := domxml.VsockCID(c); ok {
		t.Error("the clone kept the CID of its source")
	}
	if domxml.Description(c) != domxml.Description(src) {
		t.Error("the clone lost the description, and with it the LUKS marker")
	}
	if src.Name != "ubuntu-26-04" || domxml.Disks(src)[0].Source != "/var/lib/libvirt/images/ubuntu-26-04.qcow2" {
		t.Error("Clone changed its source")
	}
	text, err := c.Marshal()
	if err != nil || strings.Count(text, `<disk type="file" device="disk">`) != 2 || strings.Contains(text, `type="volume"`) {
		t.Errorf("a copied disk, the volume one too, is not a file disk: %v\n%s", err, text)
	}
}

func TestInterfaces(t *testing.T) {
	d, err := domxml.Parse(uefiDomain)
	if err != nil {
		t.Fatal(err)
	}
	got := domxml.Interfaces(d)
	if len(got) != 2 || got[0] != (domxml.Interface{MAC: "52:54:00:aa:bb:cc", Network: "default"}) || got[1].MAC != "" {
		t.Errorf("Interfaces = %+v", got)
	}
}
