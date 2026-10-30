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
	want := []string{"PoolInfo werft", "DefinePool werft", "BuildPool werft", "PoolInfo werft", "StartPool werft", "RefreshPool werft"}
	if got := f.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q", got)
	}
	if info, _ := f.PoolInfo("werft"); !info.Active {
		t.Error("pool is not active")
	}
}

func TestEnsurePoolOnAnActivePoolOnlyRefreshes(t *testing.T) {
	f := virttest.New()
	if err := virt.EnsurePool(f, "werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	before := len(f.Calls())
	if err := virt.EnsurePool(f, "werft", "/images/werft"); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls()[before:]; !reflect.DeepEqual(got, []string{"PoolInfo werft", "RefreshPool werft"}) {
		t.Errorf("calls = %q", got)
	}
}

func TestEnsurePoolPassesFailures(t *testing.T) {
	f := virttest.New()
	f.FailOn("BuildPool", failure.New(failure.Failed, "permission denied"))
	if err := virt.EnsurePool(f, "werft", "/images/werft"); !failure.Is(err, failure.Failed) {
		t.Errorf("EnsurePool = %v", err)
	}
}
