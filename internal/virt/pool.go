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

// NVRAMPool is the pool werft makes for NVRAMDir when no pool covers it.
const NVRAMPool = "nvram"

// FindNVRAM is the pool and name of the NVRAM volume at path, and changes
// nothing. For a file in NVRAMDir that no pool lists, pool is empty:
// NVRAMVolume makes a pool list it.
func FindNVRAM(h Hypervisor, path string) (pool, name string, err error) {
	pool, name, err = h.VolumeByPath(path)
	if failure.Is(err, failure.NotFound) && filepath.Dir(path) == NVRAMDir {
		return "", filepath.Base(path), nil
	}
	return pool, name, err
}

// NVRAMVolume is the pool and name of the NVRAM volume at path. libvirt
// keeps the NVRAM files of NVRAMDir in no pool by itself, and a pool lists a
// new file only after a refresh. So for a file there that no pool lists,
// NVRAMVolume refreshes the pool on NVRAMDir, or makes NVRAMPool when no
// pool covers it, and looks again. It starts an inactive pool on NVRAMDir.
func NVRAMVolume(h Hypervisor, path string) (pool, name string, err error) {
	pool, name, err = h.VolumeByPath(path)
	if !failure.Is(err, failure.NotFound) || filepath.Dir(path) != NVRAMDir {
		return pool, name, err
	}
	pool, err = nvramPool(h)
	if err != nil {
		return "", "", err
	}
	if err := EnsurePool(h, pool, NVRAMDir); err != nil {
		return "", "", failure.Wrap(failure.KindOf(err), err, "make the storage pool %s for %s", pool, NVRAMDir)
	}
	if err := h.RefreshPool(pool); err != nil {
		return "", "", err
	}
	return h.VolumeByPath(path)
}

// nvramPool is the pool that covers NVRAMDir, or NVRAMPool when none does.
// libvirt allows one pool per directory.
func nvramPool(h Hypervisor) (string, error) {
	pools, err := h.Pools()
	if err != nil {
		return "", err
	}
	for _, p := range pools {
		if filepath.Clean(p.Path) == NVRAMDir {
			return p.Name, nil
		}
	}
	return NVRAMPool, nil
}
