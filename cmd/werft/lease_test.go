package main

import (
	"strings"
	"testing"
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
