package lease_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/lease"
)

// TestReleaseKeepsTheDomainWhenTheOverlayStays: the domain is the only way
// to find the overlay again, so a failed overlay delete keeps it, and the
// next release ends the work.
func TestReleaseKeepsTheDomainWhenTheOverlayStays(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l, err := lease.Start(e, record(), "agent")
	must(t, err)
	f.FailOn("DeleteVolume", failure.New(failure.Failed, "injected"))
	if _, err := lease.Release(f, e.Dir, "werft-", l.ID); err == nil {
		t.Fatal("Release with a failing overlay delete succeeded")
	}
	if _, err := f.Domain(l.Domain); err != nil {
		t.Errorf("Release undefined %s although its overlay stays: %v", l.Domain, err)
	}
	f.FailOn("DeleteVolume", nil)
	if name, err := lease.Release(f, e.Dir, "werft-", l.ID); err != nil || name != l.Domain {
		t.Fatalf("second Release = %q, %v", name, err)
	}
	if volumes, _ := f.Volumes("werft"); slices.Contains(volumes, l.Domain+".qcow2") {
		t.Errorf("the overlay is left: %v", volumes)
	}
	if _, err := f.Domain(l.Domain); !failure.Is(err, failure.NotFound) {
		t.Errorf("%s is left: %v", l.Domain, err)
	}
	if _, err := os.Stat(e.Dir.Lease(l.ID)); !os.IsNotExist(err) {
		t.Errorf("the lease directory is left: %v", err)
	}
}

// TestStartNamesALeftOverlay: a define that fails leaves no domain, so
// werft release cannot find an overlay whose delete failed; the error names
// the command that deletes it.
func TestStartNamesALeftOverlay(t *testing.T) {
	f := world(t)
	f.FailOn("DefineDomain", failure.New(failure.Failed, "injected"))
	f.FailOn("DeleteVolume", failure.New(failure.Failed, "injected"))
	_, err := lease.Start(env(t, f, &shell{up: true}), record(), "agent")
	if !failure.Is(err, failure.Failed) || !strings.Contains(err.Error(), "virsh -c qemu:///system vol-delete --pool werft werft-vm-plain-") ||
		strings.Contains(err.Error(), "werft release") {
		t.Errorf("Start = %v, want the vol-delete command of the overlay", err)
	}
}

// TestStartWithoutVsockCID: a guest with systemd 256 or later is reached
// over vsock only, so a running lease without a CID fails and is released.
func TestStartWithoutVsockCID(t *testing.T) {
	f := world(t)
	f.FailOn("DomainXML", failure.New(failure.Failed, "injected"))
	e := env(t, f, &shell{up: true})
	if _, err := lease.Start(e, record(), "agent"); !failure.Is(err, failure.Failed) || !strings.Contains(err.Error(), "no vsock CID") {
		t.Fatalf("Start = %v, want failed for the missing vsock CID", err)
	}
	if domains, _ := f.Domains(); len(domains) != 0 {
		t.Errorf("domains left: %v", domains)
	}
	if volumes, _ := f.Volumes("werft"); len(volumes) != 2 {
		t.Errorf("volumes left: %v", volumes)
	}
	if entries, _ := os.ReadDir(e.Dir.Leases()); len(entries) != 0 {
		t.Errorf("lease directories left: %v", entries)
	}
}

// TestStartRefusesATakenLeaseDirectory: the directory of another lease with
// the same ID stays, and nothing is created.
func TestStartRefusesATakenLeaseDirectory(t *testing.T) {
	defer lease.SetNewID("abc123")()
	f := world(t)
	e := env(t, f, &shell{up: true})
	must(t, os.MkdirAll(e.Dir.Lease("abc123"), 0o700))
	before := len(f.Calls())
	if _, err := lease.Start(e, record(), "agent"); !failure.Is(err, failure.Conflict) {
		t.Fatalf("Start = %v, want conflict", err)
	}
	for _, c := range f.Calls()[before:] {
		if strings.HasPrefix(c, "CreateVolume") || strings.HasPrefix(c, "DefineDomain") {
			t.Errorf("a refused lease called %s", c)
		}
	}
	if _, err := os.Stat(e.Dir.Lease("abc123")); err != nil {
		t.Errorf("the other lease directory is gone: %v", err)
	}
}
