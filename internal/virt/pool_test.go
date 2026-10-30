package virt_test

import (
	"reflect"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

func TestEnsurePoolCreatesAMissingPool(t *testing.T) {
	f := virttest.New()
	if err := virt.EnsurePool(f, "werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	want := []string{"PoolInfo werft", "DefinePool werft", "BuildPool werft", "PoolInfo werft", "StartPool werft", "PoolInfo werft"}
	if got := f.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q", got)
	}
	if info, _ := f.PoolInfo("werft"); !info.Active {
		t.Error("pool is not active")
	}
}

func TestEnsurePoolOnAnActivePoolOnlyReadsIt(t *testing.T) {
	f := virttest.New()
	if err := virt.EnsurePool(f, "werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	before := len(f.Calls())
	if err := virt.EnsurePool(f, "werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls()[before:]; !reflect.DeepEqual(got, []string{"PoolInfo werft"}) {
		t.Errorf("calls = %q", got)
	}
}

// racingRun is a hypervisor on which another run defines and starts the
// pool just before this run does, so this run's step fails with conflict.
type racingRun struct{ *virttest.Fake }

func (r racingRun) DefinePool(name, path string) error {
	_ = r.Fake.DefinePool(name, path)
	return r.Fake.DefinePool(name, path)
}

func (r racingRun) StartPool(name string) error {
	_ = r.Fake.StartPool(name)
	return r.Fake.StartPool(name)
}

func TestEnsurePoolAcceptsAPoolAnotherRunCreates(t *testing.T) {
	f := virttest.New()
	if err := virt.EnsurePool(racingRun{f}, "werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	if info, _ := f.PoolInfo("werft"); !info.Active {
		t.Error("pool is not active")
	}
}

func TestEnsurePoolAcceptsAPoolAnotherRunStarts(t *testing.T) {
	f := virttest.New()
	if err := f.DefinePool("werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	if err := virt.EnsurePool(racingRun{f}, "werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	if info, _ := f.PoolInfo("werft"); !info.Active {
		t.Error("pool is not active")
	}
}

func TestEnsurePoolPassesFailures(t *testing.T) {
	f := virttest.New()
	f.FailOn("BuildPool", failure.New(failure.Failed, "permission denied"))
	if err := virt.EnsurePool(f, "werft", "/images/werft"); !failure.Is(err, failure.Failed) {
		t.Errorf("EnsurePool = %v", err)
	}
}

func TestEnsurePoolFailsWhenThePoolStaysInactive(t *testing.T) {
	f := virttest.New()
	f.FailOn("StartPool", failure.New(failure.Conflict, "pool is busy"))
	if err := virt.EnsurePool(f, "werft", "/images/werft"); !failure.Is(err, failure.Failed) {
		t.Errorf("EnsurePool = %v", err)
	}
}
