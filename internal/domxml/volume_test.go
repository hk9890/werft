package domxml_test

import (
	"testing"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
)

// libvirt allows an & in a domain name; the volume XML escapes it.
func TestVolumeEscapesTheName(t *testing.T) {
	var v libvirtxml.StorageVolume
	if err := v.Unmarshal(domxml.Volume("a&b.qcow2", 1024, "qcow2")); err != nil {
		t.Fatal(err)
	}
	if v.Name != "a&b.qcow2" || v.Capacity.Value != 1024 || v.Capacity.Unit != "bytes" || v.Target.Format.Type != "qcow2" {
		t.Errorf("volume = %+v", v)
	}
}
