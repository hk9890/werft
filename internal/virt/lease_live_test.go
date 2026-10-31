//go:build live

package virt_test

import (
	"testing"
	"time"

	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/virt"
)

// TestLeaseOutlivesTheDomainLive proves what start and prepare rely on:
// libvirt keeps the lease of a domain that was forced off, and a new boot
// renews it with a later expiry.
func TestLeaseOutlivesTheDomainLive(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-lease-renew")
	g.Boot()
	first := waitLease(t, h, g.Name, nil)

	livetest.Must(t, h.DestroyDomain(g.Name), "force off")
	stale := virt.DomainLeases(h, g.Name)
	if len(stale) == 0 {
		t.Fatal("control: libvirt dropped the lease of the forced-off domain; the test proves nothing")
	}
	if virt.NewLease(first, stale) {
		t.Errorf("the lease of the forced-off domain counts as new: %+v after %+v", stale, first)
	}

	livetest.Must(t, h.StartDomain(g.Name), "start again")
	if virt.NewLease(stale, virt.DomainLeases(h, g.Name)) {
		t.Error("a lease counts as new before the guest booted")
	}
	waitLease(t, h, g.Name, stale)
}

// waitLease waits until the domain holds a lease that is new against before.
func waitLease(t *testing.T, h virt.Hypervisor, name string, before []virt.DHCPLease) []virt.DHCPLease {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		now := virt.DomainLeases(h, name)
		if virt.NewLease(before, now) {
			return now
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lease newer than %+v within 2m; leases %+v", before, now)
		}
		time.Sleep(2 * time.Second)
	}
}
