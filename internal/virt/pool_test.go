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

// nvramFile is a host where the NVRAM file x_VARS.fd lies in NVRAMDir, and a
// pool lists it only after a refresh.
type nvramFile struct{ *virttest.Fake }

func (h nvramFile) RefreshPool(name string) error {
	if err := h.Fake.RefreshPool(name); err != nil {
		return err
	}
	_, err := h.CreateVolume(name, "<volume><name>x_VARS.fd</name><capacity>540672</capacity></volume>")
	return err
}

func TestNVRAMVolumeMakesThePool(t *testing.T) {
	h := nvramFile{virttest.New()}
	pool, name, err := virt.NVRAMVolume(h, virt.NVRAMDir+"/x_VARS.fd")
	if err != nil || pool != virt.NVRAMPool || name != "x_VARS.fd" {
		t.Fatalf("NVRAMVolume = %q, %q, %v", pool, name, err)
	}
	if info, _ := h.PoolInfo(virt.NVRAMPool); !info.Active {
		t.Error("the nvram pool is not active")
	}
}

func TestNVRAMVolumeRefreshesThePoolThatCoversTheDirectory(t *testing.T) {
	h := nvramFile{virttest.New()}
	if err := virt.EnsurePool(h, "qemu-nvram", virt.NVRAMDir); err != nil {
		t.Fatal(err)
	}
	pool, name, err := virt.NVRAMVolume(h, virt.NVRAMDir+"/x_VARS.fd")
	if err != nil || pool != "qemu-nvram" || name != "x_VARS.fd" {
		t.Fatalf("NVRAMVolume = %q, %q, %v", pool, name, err)
	}
	if _, err := h.PoolInfo(virt.NVRAMPool); !failure.Is(err, failure.NotFound) {
		t.Errorf("NVRAMVolume made a second pool on %s: %v", virt.NVRAMDir, err)
	}
}

// NotFound says that no pool lists the file; a pool that went away is a
// failure of its own.
func TestNVRAMVolumePoolErrorIsNoNotFound(t *testing.T) {
	h := nvramFile{virttest.New()}
	if err := virt.EnsurePool(h, virt.NVRAMPool, virt.NVRAMDir); err != nil {
		t.Fatal(err)
	}
	h.FailOn("RefreshPool", failure.New(failure.NotFound, "no storage pool"))
	_, _, err := virt.NVRAMVolume(h, virt.NVRAMDir+"/x_VARS.fd")
	if err == nil || failure.Is(err, failure.NotFound) {
		t.Fatalf("err = %v, want a failure that is not NotFound", err)
	}
}

func TestNVRAMVolumeElsewhereMakesNoPool(t *testing.T) {
	f := virttest.New()
	if _, _, err := virt.NVRAMVolume(f, "/other/x_VARS.fd"); !failure.Is(err, failure.NotFound) {
		t.Fatalf("err = %v", err)
	}
	if got := f.Calls(); !reflect.DeepEqual(got, []string{"VolumeByPath /other/x_VARS.fd"}) {
		t.Errorf("calls = %q", got)
	}
}

func TestFindNVRAMChangesNothing(t *testing.T) {
	f := virttest.New()
	pool, name, err := virt.FindNVRAM(f, virt.NVRAMDir+"/x_VARS.fd")
	if err != nil || pool != "" || name != "x_VARS.fd" {
		t.Fatalf("FindNVRAM = %q, %q, %v", pool, name, err)
	}
	if _, _, err := virt.FindNVRAM(f, "/other/x_VARS.fd"); !failure.Is(err, failure.NotFound) {
		t.Errorf("elsewhere: %v", err)
	}
	if got := f.Calls(); !reflect.DeepEqual(got, []string{"VolumeByPath " + virt.NVRAMDir + "/x_VARS.fd", "VolumeByPath /other/x_VARS.fd"}) {
		t.Errorf("calls = %q", got)
	}
}
