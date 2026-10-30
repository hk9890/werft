package state_test

import (
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/state"
)

func TestRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg")
	if r, _ := state.Root(); r != "/xdg/werft" {
		t.Errorf("Root = %q", r)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	r, _ := state.Root()
	if r != "/home/u/.local/state/werft" {
		t.Errorf("Root = %q", r)
	}
	if r.Lease("abc") != "/home/u/.local/state/werft/leases/abc" || r.Images() != "/home/u/.local/state/werft/images.toml" {
		t.Errorf("paths = %q, %q", r.Lease("abc"), r.Images())
	}
}

func TestDomainLockRefusesASecondHolder(t *testing.T) {
	d := state.Dir(t.TempDir())
	l, err := d.LockDomain("ubuntu-26-04")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.LockDomain("ubuntu-26-04"); !failure.Is(err, failure.Conflict) {
		t.Fatalf("second LockDomain = %v, want conflict", err)
	}
	other, err := d.LockDomain("ubuntu-26-04-full")
	if err != nil {
		t.Fatalf("lock of another domain: %v", err)
	}
	_ = other.Release()
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := d.LockDomain("ubuntu-26-04")
	if err != nil {
		t.Fatalf("LockDomain after release: %v", err)
	}
	_ = again.Release()
}

func TestDomainLockRejectsPathNames(t *testing.T) {
	d := state.Dir(t.TempDir())
	for _, name := range []string{"", "../x", "a/b", ".hidden", "global"} {
		if _, err := d.LockDomain(name); !failure.Is(err, failure.Usage) {
			t.Errorf("LockDomain(%q) = %v, want usage", name, err)
		}
	}
}

func TestGlobalLockWaits(t *testing.T) {
	d := state.Dir(t.TempDir())
	first, err := d.LockGlobal()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error)
	go func() {
		l, err := d.LockGlobal()
		if err == nil {
			_ = l.Release()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("second LockGlobal did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	_ = first.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second LockGlobal did not get the lock after release")
	}
}
