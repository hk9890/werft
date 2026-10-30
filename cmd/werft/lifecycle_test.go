package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

const lifecycleVM = `<domain type='kvm'><name>vm1</name><os><type>hvm</type></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm1.qcow2'/><target dev='vda'/></disk>
<interface type='network'><mac address='52:54:00:00:00:01'/><source network='default'/></interface>
</devices></domain>`

type lifecycleHost struct {
	fake    *virttest.Fake
	viewed  []string
	viewErr error
}

func newLifecycleHost(t *testing.T) *lifecycleHost {
	t.Helper()
	f := virttest.New()
	if err := virt.EnsurePool(f, "default", "/img"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateVolume("default", "<volume><name>vm1.qcow2</name><capacity unit='G'>20</capacity></volume>"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DefineDomain(lifecycleVM); err != nil {
		t.Fatal(err)
	}
	return &lifecycleHost{fake: f}
}

func (lh *lifecycleHost) run(args ...string) result {
	var out, errOut bytes.Buffer
	c := &cli{
		stdin: strings.NewReader(""), stdout: &out, stderr: &errOut,
		dial:  func() (virt.Hypervisor, error) { return lh.fake, nil },
		sleep: func(time.Duration) {},
		openViewer: func(domain string) error {
			lh.viewed = append(lh.viewed, domain)
			return lh.viewErr
		},
	}
	code := c.run(args)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func TestListText(t *testing.T) {
	lh := newLifecycleHost(t)
	r := lh.run("list")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 2 {
		t.Fatalf("list = %+v", r)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "NAME STATE USED SIZE RESET LUKS ADDRESS" {
		t.Errorf("header = %q", lines[0])
	}
	if got := strings.Join(strings.Fields(lines[1]), " "); got != "vm1 shut off - 20.00 GiB 0 - -" {
		t.Errorf("row = %q", got)
	}
}

func TestListJSON(t *testing.T) {
	lh := newLifecycleHost(t)
	r := lh.run("--json", "list")
	var doc []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 0 || len(doc) != 1 || doc[0]["name"] != "vm1" || doc[0]["size_bytes"] != float64(20<<30) || doc[0]["luks"] != false {
		t.Errorf("list --json = %+v", doc)
	}

	empty := &lifecycleHost{fake: virttest.New()}
	if r := empty.run("--json", "list"); r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("list --json without domains = %+v", r)
	}
}

func TestStartWaitsAndPrintsTheAddress(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.fake.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.5"})
	r := lh.run("start", "vm1")
	if r.code != 0 || r.stdout != "address: 192.168.122.5   ssh 192.168.122.5\n" || !strings.Contains(r.stderr, "vm1 started (headless, no window)") {
		t.Errorf("start = %+v", r)
	}
	if len(lh.viewed) != 0 {
		t.Error("start without --view opened a viewer")
	}

	r = lh.run("--json", "start", "--view", "vm1")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 0 || doc["already_running"] != true || doc["address"] != "192.168.122.5" || !strings.Contains(r.stderr, "already running") {
		t.Errorf("start --view of a running VM = %+v", r)
	}
	if len(lh.viewed) != 1 || lh.viewed[0] != "vm1" || !strings.Contains(r.stderr, "Shift+F11 fullscreen, Shift+F12 release cursor") {
		t.Errorf("viewer = %q, stderr %q", lh.viewed, r.stderr)
	}
}

func TestStartWithoutAddressOrViewerStillSucceeds(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.viewErr = errors.New("no display here")
	r := lh.run("start", "--view", "vm1")
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "No address yet") || !strings.Contains(r.stderr, "no viewer window: no display here") {
		t.Errorf("start = %+v", r)
	}
}

func TestStartTellsAboutTheDiskPassword(t *testing.T) {
	lh := newLifecycleHost(t)
	if err := lh.fake.SetDomainDescription("vm1", "vm: the disk asks for the guest password at boot"); err != nil {
		t.Fatal(err)
	}
	r := lh.run("start", "vm1")
	if r.code != 0 || !strings.Contains(r.stderr, "werft does not type it") || len(lh.fake.SentKeys("vm1")) != 0 {
		t.Errorf("start of a LUKS VM = %+v", r)
	}
}

func TestStop(t *testing.T) {
	lh := newLifecycleHost(t)
	if r := lh.run("stop", "vm1"); r.code != 0 || r.stdout != "vm1 is already shut off\n" {
		t.Errorf("stop of a shut-off VM = %+v", r)
	}
	if err := lh.fake.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	if r := lh.run("stop", "vm1"); r.code != 0 || r.stdout != "vm1 is shut off\n" || !strings.Contains(r.stderr, "ACPI shutdown sent") {
		t.Errorf("stop = %+v", r)
	}
	if err := lh.fake.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	r := lh.run("--json", "stop", "-f", "vm1")
	var doc map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 0 || doc["result"] != "forced_off" || doc["state"] != "shut off" {
		t.Errorf("stop -f --json = %+v", doc)
	}
}

func TestLifecycleExitCodes(t *testing.T) {
	lh := newLifecycleHost(t)
	if err := lh.fake.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	lh.fake.IgnoreShutdown("vm1")
	tests := []struct {
		args []string
		code int
	}{
		{[]string{"stop", "vm1"}, 7},
		{[]string{"stop", "none"}, 3},
		{[]string{"start", "none"}, 3},
		{[]string{"stop"}, 2},
		{[]string{"start", "a", "b"}, 2},
		{[]string{"list", "extra"}, 2},
		{[]string{"stop", "--bogus", "vm1"}, 2},
	}
	for _, tt := range tests {
		if r := lh.run(tt.args...); r.code != tt.code {
			t.Errorf("%q = %+v, want exit %d", tt.args, r, tt.code)
		}
	}
}
