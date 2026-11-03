package domxml_test

import (
	"fmt"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
)

func TestLeaseRefuses(t *testing.T) {
	tmpl := `<domain type='kvm'><name>s</name><os><type>hvm</type>%s</os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/werft/s.qcow2'/><target dev='vda'/></disk>
</devices></domain>`
	loader := "<loader readonly='yes' type='pflash'>/code.fd</loader>"
	tests := map[string]struct{ xml, disk string }{
		"no loader":     {fmt.Sprintf(tmpl, ""), "/werft/s.qcow2"},
		"no stock disk": {fmt.Sprintf(tmpl, loader), "/werft/other.qcow2"},
	}
	for name, tt := range tests {
		d, err := domxml.Parse(tt.xml)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := domxml.Lease(d, "werft-s-plain-abc123", tt.disk, "/werft/o.qcow2", "/werft/s_VARS.qcow2", "", nil); err == nil {
			t.Errorf("%s: Lease succeeded", name)
		}
	}
}
