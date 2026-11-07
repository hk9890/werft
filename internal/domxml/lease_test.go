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

// TestResize: memory and vCPUs reach the definition, current memory and
// the CPU topology included; the placement stays. Zero keeps a value.
func TestResize(t *testing.T) {
	const stock = `<domain type='kvm'><name>s</name><memory unit='KiB'>2097152</memory><currentMemory unit='KiB'>2097152</currentMemory>
<vcpu placement='static'>2</vcpu><cpu mode='host-passthrough'><topology sockets='1' dies='1' cores='2' threads='1'/></cpu></domain>`
	d, err := domxml.Parse(stock)
	if err != nil {
		t.Fatal(err)
	}
	if err := domxml.Resize(d, 8<<30, 6); err != nil {
		t.Fatal(err)
	}
	if n, err := domxml.Memory(d); err != nil || n != 8<<30 {
		t.Errorf("memory = %d, %v; want 8 GiB", n, err)
	}
	if c := d.CurrentMemory; c == nil || c.Value != 8<<20 || c.Unit != "KiB" {
		t.Errorf("current memory = %+v, want 8 GiB", c)
	}
	if v := d.VCPU; v.Value != 6 || v.Placement != "static" {
		t.Errorf("vcpu = %+v, want 6 with static placement", v)
	}
	if top := d.CPU.Topology; top.Sockets != 1 || top.Cores != 6 || top.Threads != 1 || top.Dies != 0 {
		t.Errorf("topology = %+v, want one socket with 6 cores", top)
	}

	d, _ = domxml.Parse(stock)
	if err := domxml.Resize(d, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := domxml.Memory(d); n != 2<<30 || d.VCPU.Value != 2 || d.CPU.Topology.Cores != 2 {
		t.Errorf("Resize with zeros changed the definition: memory %d, %+v, %+v", n, d.VCPU, d.CPU.Topology)
	}
}

func TestResizeRefuses(t *testing.T) {
	for name, xml := range map[string]string{
		"NUMA cells": `<domain><name>s</name><memory>1048576</memory><cpu><numa><cell id='0' cpus='0-1' memory='1048576'/></numa></cpu></domain>`,
		"maxMemory":  `<domain><name>s</name><maxMemory slots='16'>16777216</maxMemory><memory>1048576</memory></domain>`,
	} {
		d, err := domxml.Parse(xml)
		if err != nil {
			t.Fatal(err)
		}
		if err := domxml.Resize(d, 2<<30, 0); err == nil {
			t.Errorf("%s: Resize succeeded", name)
		}
	}
}
