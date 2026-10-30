package virt

import (
	"path/filepath"

	"github.com/hk9890/werft/internal/failure"
)

// EnsurePool makes the directory pool name at path exist, built and active.
// Runs that call it at the same time all succeed: a conflict means another
// run did the step first.
func EnsurePool(h Hypervisor, name, path string) error {
	info, err := h.PoolInfo(name)
	if failure.Is(err, failure.NotFound) {
		if err := doneByAnyone(h.DefinePool(name, path)); err != nil {
			return err
		}
		if err := doneByAnyone(h.BuildPool(name)); err != nil {
			return err
		}
		info, err = h.PoolInfo(name)
	}
	if err != nil || info.Active {
		return err
	}
	if err := doneByAnyone(h.StartPool(name)); err != nil {
		return err
	}
	if info, err = h.PoolInfo(name); err == nil && !info.Active {
		err = failure.New(failure.Failed, "pool %s is not active after start", name)
	}
	return err
}

func doneByAnyone(err error) error {
	if failure.Is(err, failure.Conflict) {
		return nil
	}
	return err
}

// NVRAMDir is where libvirt keeps the NVRAM files of its domains.
const NVRAMDir = "/var/lib/libvirt/qemu/nvram"

// NVRAMPool is the pool werft makes for NVRAMDir when no pool lists it.
const NVRAMPool = "nvram"

// NVRAMVolume is the pool and name of the NVRAM volume at path. libvirt
// keeps the NVRAM files of NVRAMDir in no pool by itself, and a pool lists a
// new file only after a refresh. So for a file there that no pool lists,
// NVRAMVolume makes NVRAMPool exist, refreshes it and looks again.
func NVRAMVolume(h Hypervisor, path string) (pool, name string, err error) {
	pool, name, err = h.VolumeByPath(path)
	if !failure.Is(err, failure.NotFound) || filepath.Dir(path) != NVRAMDir {
		return pool, name, err
	}
	if err := EnsurePool(h, NVRAMPool, NVRAMDir); err != nil {
		return "", "", failure.Wrap(failure.KindOf(err), err, "make the storage pool %s for %s", NVRAMPool, NVRAMDir)
	}
	if err := h.RefreshPool(NVRAMPool); err != nil {
		return "", "", err
	}
	return h.VolumeByPath(path)
}
