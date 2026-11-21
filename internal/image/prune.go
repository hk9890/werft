package image

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
)

// PruneEnv is what a prune runs with.
type PruneEnv struct {
	H virt.Hypervisor
	// Catalog is the images.toml of the stock versions.
	Catalog string
	// Leased is the stock versions that a lease domain names.
	Leased func() (map[string]bool, error)
	// Global takes the global lock.
	Global func() (release func(), err error)
	Out    io.Writer
}

// Prunable is every stock version that Prune deletes now: not the newest of
// its image and tier, named by no lease domain, and the backing file of no
// volume in its pool. A version a seal reserves has no record yet. Prunable
// changes nothing and takes no lock. It refuses a catalog that Prune cannot
// write, before prune asks.
func Prunable(e PruneEnv) ([]Record, error) {
	c, err := load(e.Catalog)
	if err != nil {
		return nil, err
	}
	if err := c.writable(e.Catalog); err != nil {
		return nil, err
	}
	newest := map[[2]string]string{}
	for _, r := range c.Images {
		newest[[2]string{r.Image, r.Tier}] = r.Name
	}
	leased, err := e.Leased()
	if err != nil {
		return nil, err
	}
	backing := map[string][]string{}
	out := []Record{}
	for _, r := range c.Images {
		if newest[[2]string{r.Image, r.Tier}] == r.Name || leased[r.Name] {
			continue
		}
		if _, ok := backing[r.Pool]; !ok {
			if backing[r.Pool], err = backingFiles(e.H, r.Pool); err != nil {
				return nil, err
			}
		}
		disk, err := e.H.VolumePath(r.Pool, r.Disk)
		if err == nil && slices.Contains(backing[r.Pool], disk) {
			continue
		}
		if err != nil && !failure.Is(err, failure.NotFound) {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// backingFiles is the backing file of every volume in pool. A pool that is
// gone has none.
func backingFiles(h virt.Hypervisor, pool string) ([]string, error) {
	volumes, err := h.Volumes(pool)
	if failure.Is(err, failure.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, v := range volumes {
		path, err := h.VolumeBacking(pool, v)
		if failure.Is(err, failure.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// Prune deletes, under the global lock, the disk, the NVRAM and the record
// of each version of planned that is still prunable, and returns them. A
// version whose volume delete fails keeps its record, so the next prune
// tries again; Prune goes on with the next and names every failure. The
// error of a prune that failed part-way names the removed versions.
func Prune(e PruneEnv, planned []Record) ([]Record, error) {
	release, err := e.Global()
	if err != nil {
		return nil, err
	}
	defer release()
	now, err := Prunable(e)
	if err != nil {
		return nil, err
	}
	removed := []Record{}
	var names []string
	var errs []error
	for _, r := range now {
		if !slices.ContainsFunc(planned, func(p Record) bool { return p.Name == r.Name }) {
			continue
		}
		if err := deleteVolumes(e.H, r); err != nil {
			errs = append(errs, err)
			continue
		}
		_, _ = fmt.Fprintf(e.Out, "Removed %s: its disk and NVRAM in pool %s\n", r.Name, r.Pool)
		removed = append(removed, r)
		names = append(names, r.Name)
	}
	if len(names) > 0 {
		if err := drop(e.Catalog, names); err != nil {
			errs = append(errs, fmt.Errorf("write %s: %w", e.Catalog, err))
		}
	}
	if len(errs) > 0 {
		if len(names) == 0 {
			return removed, failure.Wrap(failure.Failed, errors.Join(errs...), "prune")
		}
		return removed, failure.Wrap(failure.Failed, errors.Join(errs...), "prune removed %s and failed", strings.Join(names, ", "))
	}
	return removed, nil
}

// deleteVolumes deletes the disk and the NVRAM of r; a volume that is gone
// counts as deleted.
func deleteVolumes(h virt.Hypervisor, r Record) error {
	for _, v := range []string{r.Disk, r.NVRAM} {
		if err := h.DeleteVolume(r.Pool, v); err != nil && !failure.Is(err, failure.NotFound) {
			return fmt.Errorf("delete volume %s of %s in pool %s: %w", v, r.Name, r.Pool, err)
		}
	}
	return nil
}
