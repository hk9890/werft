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
	stale := leases(t, h, g.Name)
	if len(stale) == 0 {
		t.Fatal("control: libvirt dropped the lease of the forced-off domain; the test proves nothing")
	}
	if fresh := virt.NewLeases(first, stale); len(fresh) > 0 {
		t.Errorf("the lease of the forced-off domain counts as new: %+v after %+v", fresh, first)
	}

	livetest.Must(t, h.StartDomain(g.Name), "start again")
	if fresh := virt.NewLeases(stale, leases(t, h, g.Name)); len(fresh) > 0 {
		t.Errorf("lease %+v counts as new before the guest booted", fresh)
	}
	waitLease(t, h, g.Name, stale)
}

func leases(t *testing.T, h virt.Hypervisor, name string) []virt.DHCPLease {
	t.Helper()
	ls, err := virt.DomainLeases(h, name)
	livetest.Must(t, err, "leases of "+name)
	return ls
}

// waitLease waits until the domain holds a lease that is new against before.
func waitLease(t *testing.T, h virt.Hypervisor, name string, before []virt.DHCPLease) []virt.DHCPLease {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		fresh := virt.NewLeases(before, leases(t, h, name))
		if len(fresh) > 0 {
			return fresh
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lease newer than %+v within 2m", before)
		}
		time.Sleep(2 * time.Second)
	}
}
