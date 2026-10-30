package resetpoint_test

import (
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

func domain(nvramFormat, diskFormat string) string {
	return `<domain type='kvm'><name>vm</name><os>
<type arch='x86_64' machine='q35'>hvm</type>
<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
<nvram format='` + nvramFormat + `'>/nvram/vm_VARS</nvram></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='` + diskFormat + `'/><source file='/img/vm'/><target dev='vda'/></disk>
<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><source file='/iso'/><target dev='sda'/><readonly/></disk>
</devices></domain>`
}

func fake(t *testing.T, xml string) *virttest.Fake {
	f := virttest.New()
	if _, err := f.DefineDomain(xml); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGuard(t *testing.T) {
	tests := []struct {
		name   string
		xml    string
		libver virt.Version
		want   failure.Kind
	}{
		{"qcow2 NVRAM and disk", domain("qcow2", "qcow2"), virt.Version{Major: 12}, ""},
		{"raw NVRAM", domain("raw", "qcow2"), virt.Version{Major: 12}, failure.Unsupported},
		{"raw disk", domain("qcow2", "raw"), virt.Version{Major: 12}, failure.Unsupported},
		{"old libvirt", domain("qcow2", "qcow2"), virt.Version{Major: 11, Minor: 3}, failure.Unsupported},
		{"BIOS", `<domain type='kvm'><name>vm</name><devices><disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img'/><target dev='vda'/></disk></devices></domain>`, virt.Version{Major: 12}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fake(t, tt.xml)
			f.Version = tt.libver
			if got := failure.KindOf(resetpoint.Guard(f, "vm")); got != tt.want {
				t.Errorf("Guard kind = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTakeAndRevert(t *testing.T) {
	f := fake(t, domain("qcow2", "qcow2"))
	if err := resetpoint.Revert(f, "vm", "base"); !failure.Is(err, failure.NotFound) {
		t.Errorf("revert to a missing label = %v", err)
	}
	if err := resetpoint.Take(f, "vm", "base", false); err != nil {
		t.Fatal(err)
	}
	if err := resetpoint.Take(f, "vm", "base", false); !failure.Is(err, failure.Conflict) {
		t.Errorf("retake without replace = %v", err)
	}
	if err := resetpoint.Take(f, "vm", "base", true); err != nil {
		t.Errorf("retake with replace = %v", err)
	}
	if err := resetpoint.Revert(f, "vm", "base"); err != nil {
		t.Error(err)
	}
	if err := f.StartDomain("vm"); err != nil {
		t.Fatal(err)
	}
	if err := resetpoint.Revert(f, "vm", "base"); !failure.Is(err, failure.Conflict) {
		t.Errorf("revert of a running domain = %v", err)
	}
	if err := resetpoint.Take(f, "vm", "other", false); !failure.Is(err, failure.Conflict) {
		t.Errorf("snapshot of a running domain = %v", err)
	}
}
