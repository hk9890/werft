package lease_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// startAt leases a VM at the time now.
func startAt(t *testing.T, e lease.Env, now time.Time) lease.Lease {
	t.Helper()
	e.Now = func() time.Time { return now }
	l, err := lease.Start(e, record(), lease.Options{Owner: "agent"})
	must(t, err)
	return l
}

func gone(t *testing.T, f *virttest.Fake, e lease.Env, l lease.Lease) {
	t.Helper()
	if _, err := f.Domain(l.Domain); !failure.Is(err, failure.NotFound) {
		t.Errorf("%s is left: %v", l.Domain, err)
	}
	if volumes, _ := f.Volumes("werft"); slices.Contains(volumes, l.Domain+".qcow2") {
		t.Errorf("the overlay of %s is left: %v", l.Domain, volumes)
	}
	if _, err := os.Stat(e.Dir.Lease(l.ID)); !os.IsNotExist(err) {
		t.Errorf("the lease directory of %s is left: %v", l.ID, err)
	}
}

// TestReapReleasesExpiredLeases: a lease is released from its expiry on.
// Control: a lease made an hour later is not expired yet and stays.
func TestReapReleasesExpiredLeases(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	t0 := e.Now()
	old := startAt(t, e, t0)
	young := startAt(t, e, t0.Add(time.Hour))

	e.Now = func() time.Time { return t0.Add(lease.Lifetime) }
	r, err := lease.Reap(e)
	must(t, err)
	if len(r.Expired) != 1 || r.Expired[0].ID != old.ID || len(r.OrphanDomains) != 0 || len(r.OrphanDirectories) != 0 {
		t.Errorf("Reap = %+v, want the old lease expired alone", r)
	}
	gone(t, f, e, old)
	if _, err := f.Domain(young.Domain); err != nil {
		t.Errorf("Reap released %s before its expiry: %v", young.Domain, err)
	}
	if _, err := os.Stat(e.Dir.Lease(young.ID)); err != nil {
		t.Errorf("Reap removed the lease directory of %s: %v", young.ID, err)
	}
}

// TestReapReleasesOrphans: a lease domain without a lease directory and a
// lease directory without a lease domain both go. What is no lease stays:
// a domain with a lease name but no lease metadata, a domain with lease
// metadata but no lease name, an expired lease of another state directory
// (a live test), the stock volumes, and files under leases/ that are no
// lease directory.
func TestReapReleasesOrphans(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	orphan := startAt(t, e, e.Now())
	must(t, os.RemoveAll(e.Dir.Lease(orphan.ID)))
	must(t, os.MkdirAll(e.Dir.Lease("zzz999"), 0o700))
	must(t, os.MkdirAll(e.Dir.Lease("not-a-lease"), 0o700))
	must(t, os.WriteFile(e.Dir.Lease("yyy999"), nil, 0o600))
	meta := "<metadata><werft:lease xmlns:werft='" + lease.Namespace + "'><werft:expiry>2000-01-01T00:00:00Z</werft:expiry>" +
		"<werft:state>/other/werft</werft:state></werft:lease></metadata>"
	keep := []string{"werft-vm-plain-abc123", "ubuntu-26-04", "werft-vm-plain-abc124"}
	for i, name := range keep {
		m := meta
		if i == 0 {
			m = ""
		}
		_, err := f.DefineDomain("<domain type='kvm'><name>" + name + "</name>" + m + "</domain>")
		must(t, err)
	}
	// Control: the same expired lease without a state directory is a lease
	// from before the state element, and goes.
	_, err := f.DefineDomain("<domain type='kvm'><name>werft-vm-plain-abc125</name>" + strings.Replace(meta, "<werft:state>/other/werft</werft:state>", "", 1) + "</domain>")
	must(t, err)

	r, err := lease.Reap(e)
	must(t, err)
	if len(r.Expired) != 1 || r.Expired[0].Domain != "werft-vm-plain-abc125" || !slices.Equal(r.OrphanDomains, []string{orphan.Domain}) ||
		!slices.Equal(r.OrphanDirectories, []string{e.Dir.Lease("zzz999")}) {
		t.Errorf("Reap = %+v, want werft-vm-plain-abc125 expired, %s and the directory zzz999", r, orphan.Domain)
	}
	gone(t, f, e, orphan)
	for _, name := range keep {
		if _, err := f.Domain(name); err != nil {
			t.Errorf("Reap removed %s, which is no lease of prefix werft- and this state directory: %v", name, err)
		}
	}
	if volumes, _ := f.Volumes("werft"); !slices.Equal(volumes, []string{"vm-plain-20261102.qcow2", "vm-plain-20261102_VARS.qcow2"}) {
		t.Errorf("volumes = %v, want the stock volumes", volumes)
	}
	for _, name := range []string{"not-a-lease", "yyy999"} {
		if _, err := os.Stat(e.Dir.Lease(name)); err != nil {
			t.Errorf("Reap removed leases/%s: %v", name, err)
		}
	}
}

// TestReapKeepsTheDomainWhenTheOverlayStays: reap releases as release does,
// so a failed overlay delete keeps the domain and the lease directory, and
// the next reap ends the work.
func TestReapKeepsTheDomainWhenTheOverlayStays(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l := startAt(t, e, e.Now())
	e.Now = func() time.Time { return l.Expiry }
	f.FailOn("DeleteVolume", failure.New(failure.Failed, "injected"))
	if r, err := lease.Reap(e); !failure.Is(err, failure.Failed) || len(r.Expired) != 0 {
		t.Fatalf("Reap with a failing overlay delete = %+v, %v, want failed", r, err)
	}
	if _, err := f.Domain(l.Domain); err != nil {
		t.Errorf("Reap undefined %s although its overlay stays: %v", l.Domain, err)
	}
	if _, err := os.Stat(e.Dir.Lease(l.ID)); err != nil {
		t.Errorf("Reap removed the lease directory although the overlay stays: %v", err)
	}
	f.FailOn("DeleteVolume", nil)
	if r, err := lease.Reap(e); err != nil || len(r.Expired) != 1 {
		t.Fatalf("second Reap = %+v, %v", r, err)
	}
	gone(t, f, e, l)
}

func TestReapFailsWhenLibvirtFails(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	startAt(t, e, e.Now())
	f.FailOn("Domains", failure.New(failure.Failed, "injected"))
	if _, err := lease.Reap(e); !failure.Is(err, failure.Failed) {
		t.Errorf("Reap = %v, want failed", err)
	}
	if entries, _ := os.ReadDir(e.Dir.Leases()); len(entries) != 1 {
		t.Errorf("a Reap that could not list the domains removed lease directories: %v", entries)
	}
}

func TestList(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	want := startAt(t, e, e.Now())
	leases, err := lease.List(e)
	must(t, err)
	if len(leases) != 1 {
		t.Fatalf("List = %+v, want one lease", leases)
	}
	l := leases[0]
	if l.ID != want.ID || l.Domain != want.Domain || l.Image != "vm" || l.Tier != "plain" || l.Stock != want.Stock ||
		l.Owner != "agent" || l.User != "tester" || l.Address != "vsock/3" || l.Key != want.Key ||
		l.SSHConfig != want.SSHConfig || l.SSH != want.SSH || !l.Created.Equal(want.Created) || !l.Expiry.Equal(want.Expiry) ||
		l.State != "running" || want.State != "running" {
		t.Errorf("List = %+v\nwant %+v", l, want)
	}
}

// TestListShutOff: a lease the guest powered off lists as shut off and
// without the address of its last boot.
func TestListShutOff(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l := startAt(t, e, e.Now())
	f.SetState(l.Domain, virt.StateShutOff)
	leases, err := lease.List(e)
	must(t, err)
	if len(leases) != 1 || leases[0].State != "shut off" || leases[0].Address != "" {
		t.Errorf("List = %+v, want one lease in state shut off without an address", leases)
	}
}

// TestListSkipsAGoneDomain: a lease domain that is gone between the scan
// and its state read is not listed, and the other leases are.
func TestListSkipsAGoneDomain(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	vanished := startAt(t, e, e.Now())
	stays := startAt(t, e, e.Now())
	f.FailOn("DomainState "+vanished.Domain, failure.New(failure.NotFound, "injected"))
	leases, err := lease.List(e)
	must(t, err)
	if len(leases) != 1 || leases[0].ID != stays.ID {
		t.Errorf("List = %+v, want %s alone", leases, stays.ID)
	}
}

// TestReapIgnoresTheDomainState: libvirt that cannot give the state of a
// lease domain fails List and Extend with its kind, and does not stop a
// reap.
func TestReapIgnoresTheDomainState(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l := startAt(t, e, e.Now().Add(-lease.Lifetime))
	stays := startAt(t, e, e.Now())
	f.FailOn("DomainState", failure.New(failure.Failed, "injected"))
	if _, err := lease.List(e); !failure.Is(err, failure.Failed) {
		t.Errorf("List = %v, want failed", err)
	}
	if _, err := lease.Extend(e, stays.ID, time.Hour); !failure.Is(err, failure.Failed) {
		t.Errorf("Extend = %v, want failed", err)
	}
	r, err := lease.Reap(e)
	must(t, err)
	if len(r.Expired) != 1 || r.Expired[0].ID != l.ID {
		t.Errorf("Reap = %+v, want %s released", r, l.ID)
	}
}

func TestExtend(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l := startAt(t, e, e.Now())
	got, err := lease.Extend(e, l.ID, time.Hour)
	must(t, err)
	if !got.Expiry.Equal(l.Expiry.Add(time.Hour)) || !got.Created.Equal(l.Created) || got.State != "running" {
		t.Errorf("Extend = %+v, want expiry %v", got, l.Expiry.Add(time.Hour))
	}
	meta, err := f.DomainMetadata(l.Domain, lease.Namespace)
	must(t, err)
	if !strings.Contains(meta, "<expiry>2026-11-03T15:00:00Z</expiry>") || !strings.Contains(meta, "<owner>agent</owner>") ||
		!strings.Contains(meta, "<stock>vm-plain-20261102</stock>") || strings.Contains(meta, "xmlns") {
		t.Errorf("metadata = %q, want the new expiry without a namespace declaration", meta)
	}
	leases, err := lease.List(e)
	must(t, err)
	if !leases[0].Expiry.Equal(got.Expiry) {
		t.Errorf("List after Extend = %v, want %v", leases[0].Expiry, got.Expiry)
	}
}

func TestExtendRefuses(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l := startAt(t, e, e.Now())
	expired := e
	expired.Now = func() time.Time { return l.Expiry }
	tests := []struct {
		name string
		e    lease.Env
		id   string
		d    time.Duration
		want failure.Kind
	}{
		{"path", e, "../..", time.Hour, failure.Usage},
		{"zero", e, l.ID, 0, failure.Usage},
		{"negative", e, l.ID, -time.Hour, failure.Usage},
		{"unknown", e, "zzz999", time.Hour, failure.NotFound},
		{"expired", expired, l.ID, time.Hour, failure.NotFound},
	}
	before := len(f.Calls())
	for _, tt := range tests {
		if _, err := lease.Extend(tt.e, tt.id, tt.d); !failure.Is(err, tt.want) {
			t.Errorf("Extend %s = %v, want %s", tt.name, err, tt.want)
		}
	}
	for _, c := range f.Calls()[before:] {
		if strings.HasPrefix(c, "SetDomainMetadata") {
			t.Errorf("a refused Extend called %s", c)
		}
	}
	f.FailOn("SetDomainMetadata", failure.New(failure.Failed, "injected"))
	if _, err := lease.Extend(e, l.ID, time.Hour); !failure.Is(err, failure.Failed) {
		t.Errorf("Extend with a failing metadata write = %v, want failed", err)
	}
}

// defineLease defines the domain name with the werft lease elements body
// as its metadata.
func defineLease(t *testing.T, f *virttest.Fake, name, body string) {
	t.Helper()
	_, err := f.DefineDomain("<domain type='kvm'><name>" + name + "</name><metadata><werft:lease xmlns:werft='" + lease.Namespace + "'>" +
		body + "</werft:lease></metadata></domain>")
	must(t, err)
}

// TestReapKeepsAStatelessLease: a lease without a state directory has its
// lease directory in a state directory reap does not know, so it is no
// orphan. Control: an expired one goes (TestReapReleasesOrphans).
func TestReapKeepsAStatelessLease(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	defineLease(t, f, "werft-vm-plain-abc123", "<werft:expiry>2026-11-03T14:00:00Z</werft:expiry>")
	r, err := lease.Reap(e)
	must(t, err)
	if len(r.Expired)+len(r.OrphanDomains) != 0 {
		t.Errorf("Reap = %+v, want nothing", r)
	}
	if _, err := f.Domain("werft-vm-plain-abc123"); err != nil {
		t.Errorf("Reap released a stateless lease before its expiry: %v", err)
	}
	if leases, _ := lease.List(e); len(leases) != 1 {
		t.Errorf("List = %+v, want the stateless lease", leases)
	}
}

// TestReapKeepsTheDirectoryOfAForeignLease: a lease of another state
// directory is neither listed nor released, even when expired, and a lease
// directory with its ID stays: the two state directories can share leases/.
func TestReapKeepsTheDirectoryOfAForeignLease(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	defineLease(t, f, "werft-vm-plain-abc123", "<werft:expiry>2000-01-01T00:00:00Z</werft:expiry><werft:state>/other/werft</werft:state>")
	must(t, os.MkdirAll(e.Dir.Lease("abc123"), 0o700))
	r, err := lease.Reap(e)
	must(t, err)
	if len(r.Expired)+len(r.OrphanDomains)+len(r.OrphanDirectories) != 0 {
		t.Errorf("Reap = %+v, want nothing", r)
	}
	if _, err := os.Stat(e.Dir.Lease("abc123")); err != nil {
		t.Errorf("Reap removed the lease directory of a foreign lease: %v", err)
	}
	if _, err := f.Domain("werft-vm-plain-abc123"); err != nil {
		t.Errorf("Reap released a foreign lease: %v", err)
	}
	if leases, _ := lease.List(e); len(leases) != 0 {
		t.Errorf("List = %+v, want no foreign lease", leases)
	}
}

// TestReapFollowsSymlinks: a lease made through a symlink of the state
// directory belongs to the state directory itself.
func TestReapFollowsSymlinks(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	link := t.TempDir() + "/link"
	must(t, os.Symlink(string(e.Dir), link))
	made := e
	made.Dir = state.Dir(link)
	l := startAt(t, made, e.Now())
	if leases, _ := lease.List(e); len(leases) != 1 {
		t.Fatalf("List = %+v, want the lease made through the symlink", leases)
	}
	must(t, os.RemoveAll(e.Dir.Lease(l.ID)))
	if r, err := lease.Reap(e); err != nil || !slices.Equal(r.OrphanDomains, []string{l.Domain}) {
		t.Errorf("Reap = %+v, %v, want the orphan %s", r, err, l.Domain)
	}
}

// TestReapAfterARelease: werft release takes no global lock, so the domain
// can go between the scan and the release of reap; that is no failure.
func TestReapAfterARelease(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l := startAt(t, e, e.Now())
	e.Now = func() time.Time { return l.Expiry }
	f.FailOn("DomainXML", failure.New(failure.NotFound, "injected"))
	if _, err := lease.Reap(e); err != nil {
		t.Errorf("Reap of a domain that went meanwhile = %v", err)
	}
	if _, err := os.Stat(e.Dir.Lease(l.ID)); !os.IsNotExist(err) {
		t.Errorf("the lease directory is left: %v", err)
	}
}

func TestExtendRefusesALeaseWithoutExpiry(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	defineLease(t, f, "werft-vm-plain-abc123", "<werft:owner>agent</werft:owner><werft:state>"+string(e.Dir)+"</werft:state>")
	if _, err := lease.Extend(e, "abc123", time.Hour); !failure.Is(err, failure.Failed) || !strings.Contains(err.Error(), "no readable expiry") {
		t.Errorf("Extend = %v, want failed for the missing expiry", err)
	}
}

func TestListFailsOnBrokenMetadata(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	defineLease(t, f, "werft-vm-plain-abc123", "")
	must(t, f.SetDomainMetadata("werft-vm-plain-abc123", lease.Namespace, "werft", "<lease><owner>"))
	if _, err := lease.List(e); !failure.Is(err, failure.Failed) {
		t.Errorf("List = %v, want failed", err)
	}
}
