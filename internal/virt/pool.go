package virt

import "github.com/hk9890/werft/internal/failure"

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
