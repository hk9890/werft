package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
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
		{[]string{"lease", "vm", "--desktop", "--for", "20m"}, 2, "at least 30m with --desktop"},
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
		Owner: "agent", State: "running", User: "tester", Address: "vsock/3", Key: "/s/leases/abc123/id_ed25519",
		SSHConfig: "/s/leases/abc123/ssh_config", SSH: "ssh -F /s/leases/abc123/ssh_config abc123",
		Created: created, Expiry: created.Add(lease.Lifetime),
	}
	var out bytes.Buffer
	writeLease(&out, l)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for _, want := range []string{"lease: abc123", "owner: agent", "state: running", "user: tester", "address: vsock/3", "key: /s/leases/abc123/id_ed25519",
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
	t.Setenv("USER", "tester")
	t.Setenv("WERFT_OWNER", "")
	if got := leaseOwner(""); got != "tester" {
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
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"release", "--owner", "agent", "abc123"}, "not both"},
		{[]string{"release", "--owner", ""}, "needs an owner"},
		{[]string{"release", "--owner"}, "flag needs an argument"},
	} {
		if r := lh.run(append([]string{"--json"}, tt.args...)...); r.code != 2 || !strings.Contains(r.stdout, tt.want) {
			t.Errorf("werft %q = %+v, want usage with %q", tt.args, r, tt.want)
		}
	}
}

// TestReleaseOwner: werft release --owner releases every lease of that owner
// and keeps the lease of another owner and vm1.
func TestReleaseOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	setClock(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	mine := []string{addOwnedLease(t, lh, "aaa111", "claude-x", clock()), addOwnedLease(t, lh, "bbb222", "claude-x", clock())}
	other := addLease(t, lh, "ccc333", clock())

	r := lh.run("--json", "release", "--owner", "claude-x")
	var released []lease.Released
	if err := json.Unmarshal([]byte(r.stdout), &released); r.code != 0 || err != nil {
		t.Fatalf("release --owner = %+v, %v", r, err)
	}
	want := []lease.Released{{ID: "aaa111", Domain: mine[0]}, {ID: "bbb222", Domain: mine[1]}}
	if !slices.Equal(released, want) {
		t.Errorf("released = %v, want %v", released, want)
	}
	for _, name := range mine {
		if _, err := lh.fake.Domain(name); !failure.Is(err, failure.NotFound) {
			t.Errorf("%s is left: %v", name, err)
		}
	}
	for _, name := range []string{other, "vm1"} {
		if _, err := lh.fake.Domain(name); err != nil {
			t.Errorf("release --owner removed %s: %v", name, err)
		}
	}
	if r := lh.run("--json", "release", "--owner", "claude-x"); r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("second release --owner = %+v, want []", r)
	}
	if r := lh.run("release", "--owner", "claude-x"); r.code != 0 || r.stdout != "no lease of owner claude-x\n" {
		t.Errorf("release --owner text = %+v", r)
	}
}

// TestReleaseOwnerFails: a release that fails does not stop the others.
// The error names the lease that failed, and stderr names the lease that
// was released.
func TestReleaseOwnerFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	setClock(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	kept := addOwnedLease(t, lh, "aaa111", "claude-x", clock())
	gone := addOwnedLease(t, lh, "bbb222", "claude-x", clock())
	lh.fake.FailOn("DeleteVolume default "+kept+".qcow2", failure.New(failure.Failed, "injected"))

	r := lh.run("--json", "release", "--owner", "claude-x")
	if r.code != 1 || !strings.Contains(r.stdout, kept) || strings.Contains(r.stdout, gone) {
		t.Errorf("release --owner with one failing overlay delete = %+v, want failed naming %s only", r, kept)
	}
	if !strings.Contains(r.stderr, "Released "+gone) {
		t.Errorf("stderr = %q, want the released %s named", r.stderr, gone)
	}
	if _, err := lh.fake.Domain(gone); !failure.Is(err, failure.NotFound) {
		t.Errorf("%s is left: %v", gone, err)
	}
}

// TestReleaseOwnerKeepsForeign: release --owner keeps a lease of the same
// owner that names another state directory.
func TestReleaseOwnerKeepsForeign(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	setClock(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	mine := addOwnedLease(t, lh, "aaa111", "claude-x", clock())
	home := os.Getenv("XDG_STATE_HOME")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	foreign := addOwnedLease(t, lh, "bbb222", "claude-x", clock())
	t.Setenv("XDG_STATE_HOME", home)

	if r := lh.run("--json", "release", "--owner", "claude-x"); r.code != 0 || !strings.Contains(r.stdout, mine) || strings.Contains(r.stdout, foreign) {
		t.Errorf("release --owner = %+v, want %s only", r, mine)
	}
	if _, err := lh.fake.Domain(foreign); err != nil {
		t.Errorf("release --owner removed the foreign %s: %v", foreign, err)
	}
}

// addLease puts a lease of owner agent made at created into the fake: its
// overlay, its domain with lease metadata, and its lease directory with an
// ssh_config.
func addLease(t *testing.T, lh *lifecycleHost, id string, created time.Time) string {
	t.Helper()
	return addOwnedLease(t, lh, id, "agent", created)
}

// addOwnedLease is addLease with the owner.
func addOwnedLease(t *testing.T, lh *lifecycleHost, id, owner string, created time.Time) string {
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
		`<werft:owner>` + owner + `</werft:owner><werft:created>` + created.UTC().Format(time.RFC3339) + `</werft:created>` +
		`<werft:expiry>` + created.Add(lease.Lifetime).UTC().Format(time.RFC3339) + `</werft:expiry><werft:stock>vm-plain-20261102</werft:stock>` +
		`<werft:state>` + string(dir) + `</werft:state></werft:lease></metadata><devices><disk type='file' device='disk'><driver name='qemu' type='qcow2'/>` +
		`<source file='/img/` + name + `.qcow2'/><target dev='vda'/></disk></devices></domain>`); err != nil {
		t.Fatal(err)
	}
	lh.fake.SetState(name, virt.StateRunning)
	if err := os.MkdirAll(dir.Lease(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := guest.WriteHostConfig(filepath.Join(dir.Lease(id), "ssh_config"), id, guest.Addr{User: "tester", Host: "vsock/7"}, "key"); err != nil {
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
// lists, and lists what PRODUCT.md promises of the leases that stay. A
// shut-off lease has no address.
func TestLeasesReapsFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	old := addLease(t, lh, "old111", t0)
	addLease(t, lh, "new222", t0.Add(time.Hour))
	off := addLease(t, lh, "off333", t0.Add(time.Hour))
	lh.fake.SetState(off, virt.StateShutOff)
	setClock(t, t0.Add(lease.Lifetime))

	r := lh.run("--json", "leases")
	var leases []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &leases); r.code != 0 || err != nil {
		t.Fatalf("leases = %+v, %v", r, err)
	}
	if len(leases) != 2 {
		t.Fatalf("leases = %v, want new222 and off333", leases)
	}
	l := leases[0]
	if l["id"] != "new222" || l["owner"] != "agent" || l["image"] != "vm" || l["tier"] != "plain" || l["state"] != "running" ||
		l["address"] != "vsock/7" || l["expiry"] != "2026-11-03T15:00:00Z" {
		t.Errorf("lease = %v", l)
	}
	if l := leases[1]; l["id"] != "off333" || l["state"] != "shut off" || l["address"] != "" {
		t.Errorf("shut-off lease = %v, want state shut off and no address", l)
	}
	if _, err := lh.fake.Domain(old); !failure.Is(err, failure.NotFound) {
		t.Errorf("werft leases kept the expired %s: %v", old, err)
	}
	if !strings.Contains(r.stderr, "Released "+old) {
		t.Errorf("stderr = %q, want the released lease named", r.stderr)
	}
	if r := lh.run("leases"); r.code != 0 || !strings.Contains(r.stdout, "new222") || !strings.Contains(r.stdout, "STATE") ||
		!strings.Contains(r.stdout, "shut off") || !strings.Contains(r.stdout, "ADDRESS") {
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

// TestReapListsNoState: werft reap does not read the domain state, so an
// expired lease in its JSON has no state key.
func TestReapListsNoState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	addLease(t, lh, "abc123", t0)
	setClock(t, t0.Add(lease.Lifetime))

	r := lh.run("--json", "reap")
	var doc struct {
		Expired []map[string]any `json:"expired"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); r.code != 0 || err != nil {
		t.Fatalf("reap = %+v, %v", r, err)
	}
	if len(doc.Expired) != 1 || doc.Expired[0]["id"] != "abc123" {
		t.Fatalf("expired = %v, want abc123", doc.Expired)
	}
	if s, ok := doc.Expired[0]["state"]; ok {
		t.Errorf("expired lease has state %q, want no state key", s)
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

// stockHost is a lifecycle host with 8 GiB of memory, memory_budget_mib
// budget in config.toml, and the stock image of imagesTOML with a domain
// definition of 2 GiB.
func stockHost(t *testing.T, budget int) *lifecycleHost {
	t.Helper()
	state, config := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", config)
	lh := newLifecycleHost(t)
	lh.fake.TotalMemory = 8 << 30
	if err := virt.EnsurePool(lh.fake, "werft", "/werft"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"ubuntu-26-04-plain-20261102.qcow2", "ubuntu-26-04-plain-20261102_VARS.qcow2"} {
		if _, err := lh.fake.CreateVolume("werft", "<volume><name>"+v+"</name><capacity unit='G'>20</capacity></volume>"); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(state, "werft", "images.toml"):  imagesTOML + "domain = \"<domain type='kvm'><name>ubuntu-26-04</name><memory unit='GiB'>2</memory></domain>\"\n",
		filepath.Join(config, "werft", "config.toml"): fmt.Sprintf("memory_budget_mib = %d\n", budget),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return lh
}

// noCapacity runs werft --json args on lh and wants exit 6 with kind
// no_capacity and every one of want in the message, before the lease makes
// a volume or a domain.
func noCapacity(t *testing.T, lh *lifecycleHost, args []string, want ...string) {
	t.Helper()
	before := len(lh.fake.Calls())
	r := lh.run(append([]string{"--json"}, args...)...)
	var doc struct {
		Error struct{ Kind, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || r.code != 6 || doc.Error.Kind != string(failure.NoCapacity) {
		t.Fatalf("werft %q = %+v, %v; want exit 6 with kind no_capacity", args, r, err)
	}
	for _, w := range want {
		if !strings.Contains(doc.Error.Message, w) {
			t.Errorf("werft %q: message %q lacks %q", args, doc.Error.Message, w)
		}
	}
	if calls := lh.fake.Calls()[before:]; slices.ContainsFunc(calls, func(c string) bool {
		return strings.HasPrefix(c, "CreateVolume") || strings.HasPrefix(c, "DefineDomain")
	}) {
		t.Errorf("a refused lease made a volume or a domain: %v", calls)
	}
}

// TestLeaseOverTheBudget: memory_budget_mib in config.toml reaches the
// admission of werft lease, and a lease over it exits 6 with kind
// no_capacity, before it makes a volume or a domain.
func TestLeaseOverTheBudget(t *testing.T) {
	lh := stockHost(t, 7168)
	noCapacity(t, lh, []string{"lease", "ubuntu-26-04"}, "the lease needs 2048 MiB", "keeps 7168 MiB")
}

// TestLeaseMemoryReachesAdmission: with --memory, admission counts the
// requested memory, also for a stock version that --stock names. Control:
// the 2 GiB of the stock definition fit the same budget, so the refusal
// comes from the flag.
func TestLeaseMemoryReachesAdmission(t *testing.T) {
	lh := stockHost(t, 5120)
	noCapacity(t, lh, []string{"lease", "ubuntu-26-04", "--memory", "4096"}, "the lease needs 4096 MiB", "keeps 5120 MiB")
	noCapacity(t, lh, []string{"lease", "--stock", "ubuntu-26-04-plain-20261102", "--memory", "4096", "ubuntu-26-04"}, "the lease needs 4096 MiB")
	lh = stockHost(t, 6144)
	noCapacity(t, lh, []string{"lease", "ubuntu-26-04", "--memory", "4096"}, "the lease needs 4096 MiB")
	lh.fake.FailOn("CreateVolume", failure.New(failure.Failed, "control: past admission"))
	if r := lh.run("--json", "lease", "ubuntu-26-04"); !strings.Contains(r.stdout, "control: past admission") {
		t.Errorf("control: a lease with the stock memory = %+v, want it past admission", r)
	}
}

// TestLeaseOptionArguments: a flag out of its bounds exits 2 before werft
// reads images.toml or calls libvirt; a --stock that does not exist exits
// 3, and one of another image, or of another tier than an explicit --tier,
// exits 2.
func TestLeaseOptionArguments(t *testing.T) {
	lh := stockHost(t, 0)
	before := len(lh.fake.Calls())
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"ubuntu-26-04", "--memory", "8"}, 2, "--memory is a size in MiB, at least 256"},
		{[]string{"ubuntu-26-04", "--memory", "0"}, 2, "--memory is a size in MiB"},
		{[]string{"ubuntu-26-04", "--memory", "8G"}, 2, "invalid value"},
		{[]string{"ubuntu-26-04", "--cpus", "0"}, 2, "--cpus is at least 1"},
		{[]string{"ubuntu-26-04", "--cpus", "-1"}, 2, "invalid value"},
		{[]string{"ubuntu-26-04", "--for", "10m"}, 2, "--for is a duration from 15m to 24h"},
		{[]string{"ubuntu-26-04", "--for", "25h"}, 2, "--for is a duration"},
		{[]string{"ubuntu-26-04", "--for", "soon"}, 2, "invalid value"},
		{[]string{"ubuntu-26-04", "--stock", "ubuntu-26-04-plain-20261030"}, 3, "no stock version ubuntu-26-04-plain-20261030"},
		{[]string{"ubuntu-26-04", "--tier", "full", "--stock", "ubuntu-26-04-plain-20261102"}, 2, "is of tier plain, not of tier full"},
		{[]string{"debian-13", "--stock", "ubuntu-26-04-plain-20261102"}, 2, "is of image ubuntu-26-04, not of image debian-13"},
	} {
		args := append([]string{"--json", "lease"}, tt.args...)
		if r := lh.run(args...); r.code != tt.code || !strings.Contains(r.stdout, tt.want) {
			t.Errorf("werft %q = %+v, want exit %d with %q", args, r, tt.code, tt.want)
		}
	}
	if calls := lh.fake.Calls()[before:]; len(calls) > 0 {
		t.Errorf("a refused lease called libvirt: %v", calls)
	}
}

// TestLeaseStockGivesTheTier: --stock of a full version without --tier
// leases that version, in a domain of tier full. The start fails, and so
// does the undefine, so the defined domain stays for the test to read.
func TestLeaseStockGivesTheTier(t *testing.T) {
	lh := stockHost(t, 0)
	saved := guestEnsureKey
	guestEnsureKey = func(path string) error { return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAA test"), 0o600) }
	t.Cleanup(func() { guestEnsureKey = saved })
	for _, v := range []string{"ubuntu-26-04-full-20261102.qcow2", "ubuntu-26-04-full-20261102_VARS.qcow2"} {
		if _, err := lh.fake.CreateVolume("werft", "<volume><name>"+v+"</name><capacity unit='G'>20</capacity></volume>"); err != nil {
			t.Fatal(err)
		}
	}
	disk, err := lh.fake.VolumePath("werft", "ubuntu-26-04-full-20261102.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	full := strings.ReplaceAll(imagesTOML, "plain", "full") + fmt.Sprintf("domain = \"<domain type='kvm'><name>ubuntu-26-04</name><memory unit='GiB'>2</memory><vcpu>2</vcpu>"+
		"<os><type>hvm</type><loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader></os>"+
		"<devices><disk type='file' device='disk'><source file='%s'/><target dev='vda' bus='virtio'/></disk></devices></domain>\"\n", disk)
	catalog := filepath.Join(os.Getenv("XDG_STATE_HOME"), "werft", "images.toml")
	plain, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, append(plain, full...), 0o600); err != nil {
		t.Fatal(err)
	}
	lh.fake.FailOn("StartDomain", failure.New(failure.Failed, "test: no start"))
	lh.fake.FailOn("UndefineDomain", failure.New(failure.Failed, "test: keep the domain"))

	if r := lh.run("--json", "lease", "ubuntu-26-04", "--stock", "ubuntu-26-04-full-20261102"); !strings.Contains(r.stdout, "test: no start") {
		t.Fatalf("werft lease --stock of a full version = %+v, want it to fail at the start", r)
	}
	domains, err := lh.fake.Domains()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range domains {
		if !strings.Contains(d.Name, "ubuntu-26-04-full-") {
			continue
		}
		meta, err := lh.fake.DomainMetadata(d.Name, lease.Namespace)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(meta, "ubuntu-26-04-full-20261102") {
			t.Errorf("metadata = %q, want stock version ubuntu-26-04-full-20261102", meta)
		}
		return
	}
	t.Errorf("domains = %v, want a lease domain of tier full", domains)
}

// TestLeaseOptionsReachTheDomain: --cpus, --memory and --for reach the
// definition of the lease domain, and --stock picks an older stock version
// over the newest one. The start fails, and so does the undefine, so the
// defined domain stays for the test to read. Control: without the flags,
// the lease has the newest version and the default lifetime.
func TestLeaseOptionsReachTheDomain(t *testing.T) {
	lh := stockHost(t, 0)
	setClock(t, time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	saved := guestEnsureKey
	guestEnsureKey = func(path string) error { return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAA test"), 0o600) }
	t.Cleanup(func() { guestEnsureKey = saved })
	for _, v := range []string{"ubuntu-26-04-plain-20261030.qcow2", "ubuntu-26-04-plain-20261030_VARS.qcow2"} {
		if _, err := lh.fake.CreateVolume("werft", "<volume><name>"+v+"</name><capacity unit='G'>20</capacity></volume>"); err != nil {
			t.Fatal(err)
		}
	}
	var catalog strings.Builder
	for _, version := range []string{"20261030", "20261102"} {
		disk, err := lh.fake.VolumePath("werft", "ubuntu-26-04-plain-"+version+".qcow2")
		if err != nil {
			t.Fatal(err)
		}
		catalog.WriteString(strings.ReplaceAll(imagesTOML, "20261102", version))
		fmt.Fprintf(&catalog, "domain = \"<domain type='kvm'><name>ubuntu-26-04</name><memory unit='GiB'>2</memory><vcpu>2</vcpu>"+
			"<os><type>hvm</type><loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader></os>"+
			"<devices><disk type='file' device='disk'><source file='%s'/><target dev='vda' bus='virtio'/></disk></devices></domain>\"\n", disk)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "werft", "images.toml"), []byte(catalog.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lh.fake.FailOn("StartDomain", failure.New(failure.Failed, "test: no start"))
	lh.fake.FailOn("UndefineDomain", failure.New(failure.Failed, "test: keep the domain"))

	// defined runs werft lease ubuntu-26-04 args and returns the definition
	// and the werft metadata of the domain it defined.
	defined := func(args ...string) (*libvirtxml.Domain, string) {
		t.Helper()
		before, err := lh.fake.Domains()
		if err != nil {
			t.Fatal(err)
		}
		if r := lh.run(append([]string{"--json", "lease", "ubuntu-26-04"}, args...)...); !strings.Contains(r.stdout, "test: no start") {
			t.Fatalf("werft lease %q = %+v, want it to fail at the start", args, r)
		}
		after, err := lh.fake.Domains()
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range after {
			if slices.Contains(before, d) {
				continue
			}
			text, err := lh.fake.DomainXML(d.Name, true)
			if err != nil {
				t.Fatal(err)
			}
			dom, err := domxml.Parse(text)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := lh.fake.DomainMetadata(d.Name, lease.Namespace)
			if err != nil {
				t.Fatal(err)
			}
			return dom, meta
		}
		t.Fatalf("werft lease %q defined no domain", args)
		return nil, ""
	}

	d, meta := defined("--cpus", "4", "--memory", "3072", "--for", "90m", "--stock", "ubuntu-26-04-plain-20261030")
	if d.VCPU == nil || d.VCPU.Value != 4 {
		t.Errorf("vcpu = %+v, want 4", d.VCPU)
	}
	if n, err := domxml.Memory(d); err != nil || n != 3<<30 {
		t.Errorf("memory = %d, %v; want 3 GiB", n, err)
	}
	for _, want := range []string{"2026-11-03T11:30:00Z", "ubuntu-26-04-plain-20261030"} {
		if !strings.Contains(meta, want) {
			t.Errorf("metadata = %q, want %q", meta, want)
		}
	}
	d, meta = defined()
	if d.VCPU == nil || d.VCPU.Value != 2 {
		t.Errorf("control: vcpu = %+v, want the 2 of the stock definition", d.VCPU)
	}
	for _, want := range []string{"2026-11-03T14:00:00Z", "ubuntu-26-04-plain-20261102"} {
		if !strings.Contains(meta, want) {
			t.Errorf("control: metadata = %q, want %q", meta, want)
		}
	}
}

// TestLeaseDesktopReadsTheRecipeGuestPassword: --desktop reads the guest
// password where guest_password of the recipe in config.toml says, before
// it makes a volume or a domain. Here the keyring has no item, so the lease
// fails with secret_unavailable and names the item of the recipe.
func TestLeaseDesktopReadsTheRecipeGuestPassword(t *testing.T) {
	lh := stockHost(t, 0)
	config := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "werft")
	recipe := filepath.Join(config, "recipe.toml")
	if err := os.WriteFile(recipe, []byte("[guest_password]\nkeyring = { service = \"werft\", username = \"tester\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.toml"), []byte("recipe = \""+recipe+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(lh.fake.Calls())
	r := lh.run("--json", "lease", "ubuntu-26-04", "--desktop")
	if r.code != 10 || !strings.Contains(r.stdout, "service=werft username=tester") || !strings.Contains(r.stdout, "--desktop unlocks the keyring") {
		t.Fatalf("lease --desktop = %+v, want exit 10 that names the keyring item of the recipe", r)
	}
	if calls := lh.fake.Calls()[before:]; slices.ContainsFunc(calls, func(c string) bool {
		return strings.HasPrefix(c, "CreateVolume") || strings.HasPrefix(c, "DefineDomain")
	}) {
		t.Errorf("a lease without the guest password made a volume or a domain: %v", calls)
	}
}

// TestStartWritesTheNewAddressOfALease: werft start of a stopped lease
// brings its ssh_config to the address of the new boot.
func TestStartWritesTheNewAddressOfALease(t *testing.T) {
	const name, id = "werft-vm-plain-abc123", "abc123"
	lh := newLifecycleHost(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := state.Root()
	if err != nil {
		t.Fatal(err)
	}
	metadata := "<metadata><werft:lease xmlns:werft='" + lease.Namespace + "'><werft:state>" + string(dir) + "</werft:state></werft:lease></metadata>"
	if _, err := lh.fake.DefineDomain(strings.Replace(lifecycleVM(lh.nvramDir), "<name>vm1</name>", "<name>"+name+"</name>"+metadata, 1)); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir.Lease(id), "ssh_config")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := guest.WriteHostConfig(config, id, guest.Addr{User: "tester", Host: "192.168.122.50"}, "key"); err != nil {
		t.Fatal(err)
	}
	lh.lease = &virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.51"}
	if r := lh.run("start", name); r.code != 0 {
		t.Fatalf("start = %+v", r)
	}
	got, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "HostName 192.168.122.51\n") {
		t.Errorf("ssh_config = %s, want the new address", got)
	}
}

// TestLeaseDesktopAt255IgnoresAMissingRecipe: --desktop at systemd 255 fails
// with unsupported, also when the recipe config.toml names does not exist.
func TestLeaseDesktopAt255IgnoresAMissingRecipe(t *testing.T) {
	lh := stockHost(t, 0)
	images := filepath.Join(os.Getenv("XDG_STATE_HOME"), "werft", "images.toml")
	content, err := os.ReadFile(images)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(images, []byte(strings.Replace(string(content), "systemd = 259", "systemd = 255", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "werft")
	missing := filepath.Join(config, "missing.toml")
	if err := os.WriteFile(filepath.Join(config, "config.toml"), []byte("recipe = \""+missing+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := lh.run("--json", "lease", "ubuntu-26-04", "--desktop")
	if r.code != 9 || !strings.Contains(r.stdout, "systemd 255") {
		t.Fatalf("lease --desktop = %+v, want exit 9 that names systemd 255", r)
	}
}
