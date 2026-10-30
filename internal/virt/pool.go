package virt

import "github.com/hk9890/werft/internal/failure"

// EnsurePool makes the directory pool name at path exist, built and active,
// and refreshes it so it lists volumes created outside the storage API.
func EnsurePool(h Hypervisor, name, path string) error {
	info, err := h.PoolInfo(name)
	if failure.Is(err, failure.NotFound) {
		if err := h.DefinePool(name, path); err != nil {
			return err
		}
		if err := h.BuildPool(name); err != nil {
			return err
		}
		info, err = h.PoolInfo(name)
	}
	if err != nil {
		return err
	}
	if !info.Active {
		if err := h.StartPool(name); err != nil {
			return err
		}
	}
	return h.RefreshPool(name)
}
