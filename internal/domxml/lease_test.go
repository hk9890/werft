package domxml_test

import (
	"fmt"
	"strings"
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

// TestLeaseDropsFileConsole: a lease gets a pty console without a log, so
// two leases of one stock definition share no console file. Control: the
// stock definition names the files, in a log and as a source.
func TestLeaseDropsFileConsole(t *testing.T) {
	const log = "/var/log/libvirt/qemu/stock-console.log"
	const file = "/werft/stock-serial.log"
	tmpl, err := domxml.Parse(`<domain type='kvm'><name>s</name><os><type>hvm</type><loader readonly='yes' type='pflash'>/code.fd</loader></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/werft/s.qcow2'/><target dev='vda'/></disk>
<serial type='pty'><log file='` + log + `' append='off'/><target port='0'/></serial>
<serial type='file'><source path='` + file + `'/><target port='1'/></serial>
<console type='pty'><log file='` + log + `'/><target type='serial' port='0'/></console>
</devices></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	stock, _ := tmpl.Marshal()
	if !strings.Contains(stock, log) || !strings.Contains(stock, file) {
		t.Fatalf("the stock definition names no console file:\n%s", stock)
	}
	d, err := domxml.Lease(tmpl, "werft-s-plain-abc123", "/werft/s.qcow2", "/werft/o.qcow2", "/werft/s_VARS.qcow2", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, log) || strings.Contains(text, file) || strings.Contains(text, "<log") {
		t.Errorf("the lease definition shares a console file:\n%s", text)
	}
	if len(d.Devices.Serials) != 2 || len(d.Devices.Consoles) != 1 {
		t.Fatalf("the lease lost a serial or console device:\n%s", text)
	}
	for _, s := range d.Devices.Serials {
		if s.Source == nil || s.Source.Pty == nil {
			t.Errorf("serial port %v is no pty:\n%s", s.Target, text)
		}
	}
	if c := d.Devices.Consoles[0]; c.Source == nil || c.Source.Pty == nil {
		t.Errorf("the console is no pty:\n%s", text)
	}
}
