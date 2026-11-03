package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/state"
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

// addLease puts a lease made at created into the fake: its overlay, its
// domain with lease metadata, and its lease directory with an ssh_config.
func addLease(t *testing.T, lh *lifecycleHost, id string, created time.Time) string {
	t.Helper()
	name := "werft-vm-plain-" + id
	if _, err := lh.fake.CreateVolume("default", "<volume><name>"+name+".qcow2</name><capacity>1</capacity></volume>"); err != nil {
		t.Fatal(err)
	}
	dir, err := state.Root()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lh.fake.DefineDomain(`<domain type='kvm'><name>` + name + `</name><metadata><werft:lease xmlns:werft='` + lease.Namespace + `'>` +
		`<werft:owner>agent</werft:owner><werft:created>` + created.UTC().Format(time.RFC3339) + `</werft:created>` +
		`<werft:expiry>` + created.Add(lease.Lifetime).UTC().Format(time.RFC3339) + `</werft:expiry><werft:stock>vm-plain-20261102</werft:stock>` +
		`<werft:state>` + string(dir) + `</werft:state></werft:lease></metadata><devices><disk type='file' device='disk'><driver name='qemu' type='qcow2'/>` +
		`<source file='/img/` + name + `.qcow2'/><target dev='vda'/></disk></devices></domain>`); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir.Lease(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := guest.WriteHostConfig(filepath.Join(dir.Lease(id), "ssh_config"), id, guest.Addr{User: "hans", Host: "vsock/7"}, "key"); err != nil {
		t.Fatal(err)
	}
	return name
}

// setClock makes clock now until the test ends.
func setClock(t *testing.T, now time.Time) {
	old := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = old })
}

// TestLeasesReapsFirst: werft leases releases an expired lease before it
// lists, and lists what PRODUCT.md promises of the lease that stays.
func TestLeasesReapsFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	old := addLease(t, lh, "old111", t0)
	addLease(t, lh, "new222", t0.Add(time.Hour))
	setClock(t, t0.Add(lease.Lifetime))

	r := lh.run("--json", "leases")
	var leases []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &leases); r.code != 0 || err != nil {
		t.Fatalf("leases = %+v, %v", r, err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %v, want new222 alone", leases)
	}
	l := leases[0]
	if l["id"] != "new222" || l["owner"] != "agent" || l["image"] != "vm" || l["tier"] != "plain" || l["address"] != "vsock/7" ||
		l["expiry"] != "2026-11-03T15:00:00Z" {
		t.Errorf("lease = %v", l)
	}
	if _, err := lh.fake.Domain(old); !failure.Is(err, failure.NotFound) {
		t.Errorf("werft leases kept the expired %s: %v", old, err)
	}
	if !strings.Contains(r.stderr, "Released "+old) {
		t.Errorf("stderr = %q, want the released lease named", r.stderr)
	}
	if r := lh.run("leases"); r.code != 0 || !strings.Contains(r.stdout, "new222") || !strings.Contains(r.stdout, "ADDRESS") {
		t.Errorf("leases text = %+v", r)
	}
}

// TestReapRemovesOrphans: werft reap removes a lease domain without a lease
// directory and a lease directory without a lease domain; vm1 is no lease
// and stays.
func TestReapRemovesOrphans(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	setClock(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	orphan := addLease(t, lh, "abc123", clock())
	dir, _ := state.Root()
	if err := os.RemoveAll(dir.Lease("abc123")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir.Lease("zzz999"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := lh.run("--json", "reap")
	var doc lease.Reaped
	if err := json.Unmarshal([]byte(r.stdout), &doc); r.code != 0 || err != nil {
		t.Fatalf("reap = %+v, %v", r, err)
	}
	if !slices.Equal(doc.OrphanDomains, []string{orphan}) || !slices.Equal(doc.OrphanDirectories, []string{dir.Lease("zzz999")}) || len(doc.Expired) != 0 {
		t.Errorf("reap = %+v", doc)
	}
	if _, err := lh.fake.Domain(orphan); !failure.Is(err, failure.NotFound) {
		t.Errorf("%s is left: %v", orphan, err)
	}
	if _, err := os.Stat(dir.Lease("zzz999")); !os.IsNotExist(err) {
		t.Errorf("the orphan lease directory is left: %v", err)
	}
	if _, err := lh.fake.Domain("vm1"); err != nil {
		t.Errorf("reap removed vm1: %v", err)
	}
	if r := lh.run("reap"); r.code != 0 || r.stdout != "nothing to reap\n" {
		t.Errorf("second reap = %+v", r)
	}
}

// TestReapFails: a release that fails makes werft reap fail, with the kind
// failed, and the other commands only warn.
func TestReapFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	addLease(t, lh, "abc123", t0)
	setClock(t, t0.Add(lease.Lifetime))
	lh.fake.FailOn("DeleteVolume", failure.New(failure.Failed, "injected"))
	if r := lh.run("--json", "reap"); r.code != 1 || !strings.Contains(r.stdout, `"kind": "failed"`) {
		t.Errorf("reap with a failing overlay delete = %+v, want failed", r)
	}
	if r := lh.run("--json", "leases"); r.code != 0 || !strings.Contains(r.stderr, "warning:") {
		t.Errorf("leases with a failing reap = %+v, want a warning and success", r)
	}
}

func TestExtendMovesTheExpiry(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	setClock(t, t0)
	addLease(t, lh, "abc123", t0)
	r := lh.run("--json", "extend", "abc123", "--for", "1h")
	if r.code != 0 || !strings.Contains(r.stdout, `"expiry": "2026-11-03T15:00:00Z"`) {
		t.Fatalf("extend = %+v", r)
	}
	r = lh.run("--json", "extend", "--for", "30m", "abc123")
	if r.code != 0 || !strings.Contains(r.stdout, `"expiry": "2026-11-03T15:30:00Z"`) {
		t.Errorf("extend with the flag first = %+v", r)
	}
	r = lh.run("extend", "abc123")
	if want := t0.Add(2*lease.Lifetime + 90*time.Minute).Local().Format("2006-01-02 15:04"); r.code != 0 || !strings.Contains(r.stdout, want) {
		t.Errorf("extend by the default = %+v, want %s", r, want)
	}
}

func TestLeaseCommandArguments(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	before := len(lh.fake.Calls())
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"leases", "x"}, "no arguments"},
		{[]string{"reap", "x"}, "no arguments"},
		{[]string{"extend"}, "one lease id"},
		{[]string{"extend", "a", "b"}, "one lease id"},
		{[]string{"extend", "abc123", "--for", "soon"}, "invalid value"},
	} {
		if r := lh.run(append([]string{"--json"}, tt.args...)...); r.code != 2 || !strings.Contains(r.stdout, tt.want) {
			t.Errorf("werft %q = %+v, want usage with %q", tt.args, r, tt.want)
		}
	}
	if calls := lh.fake.Calls()[before:]; len(calls) > 0 {
		t.Errorf("a refused command called libvirt: %v", calls)
	}
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"extend", "abc123", "--for", "-1h"}, 2, "positive duration"},
		{[]string{"extend", "abc123"}, 3, "no lease abc123"},
	} {
		if r := lh.run(append([]string{"--json"}, tt.args...)...); r.code != tt.code || !strings.Contains(r.stdout, tt.want) {
			t.Errorf("werft %q = %+v, want exit %d with %q", tt.args, r, tt.code, tt.want)
		}
	}
}
