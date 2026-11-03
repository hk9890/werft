package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/lease"
)

func TestLeaseArguments(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	before := len(lh.fake.Calls())
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"lease"}, 2, "one image name"},
		{[]string{"lease", "a", "b"}, 2, "one image name"},
		{[]string{"lease", "--tier", "gold", "vm"}, 2, "plain or full"},
		{[]string{"lease", "vm", "--tier", "full", "--owner", "agent"}, 3, "no stock image vm of tier full"},
		{[]string{"lease", "--tier", "full", "vm"}, 3, "no stock image vm of tier full"},
	}
	for _, tt := range tests {
		r := lh.run(append([]string{"--json"}, tt.args...)...)
		if r.code != tt.code || !strings.Contains(r.stdout, tt.want) {
			t.Errorf("werft %q = %+v, want exit %d with %q", tt.args, r, tt.code, tt.want)
		}
	}
	if calls := lh.fake.Calls()[before:]; len(calls) > 0 {
		t.Errorf("a refused lease called libvirt: %v", calls)
	}
}

// TestLeaseText: the text output names what PRODUCT.md promises, and ends
// with the ssh command alone.
func TestLeaseText(t *testing.T) {
	created := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	l := lease.Lease{
		ID: "abc123", Domain: "werft-vm-plain-abc123", Image: "vm", Tier: "plain", Stock: "vm-plain-20261102",
		Owner: "agent", User: "hans", Address: "vsock/3", Key: "/s/leases/abc123/id_ed25519",
		SSHConfig: "/s/leases/abc123/ssh_config", SSH: "ssh -F /s/leases/abc123/ssh_config abc123",
		Created: created, Expiry: created.Add(lease.Lifetime),
	}
	var out bytes.Buffer
	writeLease(&out, l)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for _, want := range []string{"lease: abc123", "owner: agent", "user: hans", "address: vsock/3", "key: /s/leases/abc123/id_ed25519",
		"ssh_config: /s/leases/abc123/ssh_config", "created: 2026-11-03 10:00", "expiry: 2026-11-03 14:00"} {
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.Join(strings.Fields(line), " ") == want }) {
			t.Errorf("text output lacks %q:\n%s", want, out.String())
		}
	}
	if lines[len(lines)-1] != l.SSH {
		t.Errorf("last line = %q, want the ssh command", lines[len(lines)-1])
	}
}

func TestLeaseOwner(t *testing.T) {
	t.Setenv("USER", "hans")
	t.Setenv("WERFT_OWNER", "")
	if got := leaseOwner(""); got != "hans" {
		t.Errorf("owner without flag and $WERFT_OWNER = %q, want $USER", got)
	}
	t.Setenv("WERFT_OWNER", "agent-7")
	if got := leaseOwner(""); got != "agent-7" {
		t.Errorf("owner with $WERFT_OWNER = %q", got)
	}
	if got := leaseOwner("ci"); got != "ci" {
		t.Errorf("owner with --owner = %q", got)
	}
}

func TestReleaseArguments(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	if r := lh.run("release"); r.code != 2 {
		t.Errorf("release without an id = %+v, want usage", r)
	}
	if r := lh.run("release", "../x"); r.code != 2 {
		t.Errorf("release of a path = %+v, want usage", r)
	}
	if r := lh.run("--json", "release", "abc123"); r.code != 0 || !strings.Contains(r.stdout, `"domain": ""`) {
		t.Errorf("release of a lease that is gone = %+v, want success", r)
	}
	if r := lh.run("release", "abc123"); r.code != 0 || r.stdout != "lease abc123 is gone already\n" {
		t.Errorf("release text = %+v", r)
	}
}
