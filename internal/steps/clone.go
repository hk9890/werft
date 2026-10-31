package steps

import (
	"fmt"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/guest/assets"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/virt"
)

// Clone replaces the build VM's clone, <build VM>-full, with a new copy of
// the shut-off build VM, and makes the clone the domain of the steps after
// it. The copy gets its own disk volumes in the pools of the originals, and
// the build VM's virt-manager viewer settings: virt-manager keeps them per
// domain UUID, and the clone's UUID is new.
type Clone struct{}

func target(r *pipeline.Run) string { return r.BuildVM + resetpoint.FullSuffix }

func (Clone) Check(r *pipeline.Run) error {
	if _, err := sources(r.H, r.BuildVM); err != nil {
		return err
	}
	t := target(r)
	exists, err := requireShutOff(r.H, t)
	if err != nil || !exists {
		return err
	}
	r.Ask(t + " exists and will be replaced. Everything in it is lost.")
	return nil
}

// requireShutOff fails with failure.Conflict unless the domain name is shut
// off or does not exist, and reports whether it exists.
func requireShutOff(h virt.Hypervisor, name string) (bool, error) {
	state, err := h.DomainState(name)
	if failure.Is(err, failure.NotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state != virt.StateShutOff {
		return true, failure.New(failure.Conflict, "%s is %s; provision replaces it, so it must be shut off first", name, state)
	}
	return true, nil
}

// Run refuses a running source: a copy of the disk and NVRAM of a running
// guest is inconsistent. It checks the clone again: the run took long since
// preflight, and virt-manager can have started it in between.
func (Clone) Run(r *pipeline.Run) error {
	state, err := r.H.DomainState(r.Domain)
	if err != nil {
		return err
	}
	if state != virt.StateShutOff {
		return failure.New(failure.Conflict, "%s is %s; clone copies its disk, so a poweroff step must come before clone", r.Domain, state)
	}
	t := target(r)
	exists, err := requireShutOff(r.H, t)
	if err != nil {
		return err
	}
	src, err := sources(r.H, r.Domain)
	if err != nil {
		return err
	}
	if err := src.listNVRAM(r.H); err != nil {
		return err
	}
	if exists {
		r.Printf("Removing the old %s", t)
		if err := removeClone(r.H, t); err != nil {
			return err
		}
	}
	r.Printf("Cloning %s to %s", r.Domain, t)
	from, err := r.H.Domain(r.Domain)
	if err != nil {
		return err
	}
	paths, created, err := copyDisks(r.H, src.disks, t)
	nvram := ""
	if err == nil {
		nvram, created, err = copyNVRAM(r.H, src, t, created)
	}
	if err == nil {
		err = defineClone(r.H, src.domain, t, paths, nvram)
	}
	if err != nil {
		for _, v := range created {
			_ = r.H.DeleteVolume(v.pool, v.name)
		}
		return failure.Wrap(failure.KindOf(err), err, "%s itself is intact", r.Domain)
	}
	to, err := r.H.Domain(t)
	if err != nil {
		return err
	}
	if err := r.Host.CopyViewerSettings(from.UUID, to.UUID); err != nil {
		r.Printf("Could not copy virt-manager's viewer settings to %s; its window opens with the defaults", t)
	}
	r.Domain, r.Addr = t, guest.Addr{}
	return nil
}

type poolVolume struct{ pool, name string }

// cloneSource is a domain with the storage volumes of its writable disks and
// of its NVRAM, when it has one.
type cloneSource struct {
	domain      *libvirtxml.Domain
	disks       []domxml.Disk
	nvram       *poolVolume
	nvramPath   string
	nvramFormat string
}

// sources reads the domain and finds the volume of each writable disk and of
// the NVRAM, and changes nothing. A disk or NVRAM outside every storage pool
// fails. An NVRAM in virt.NVRAMDir that no pool lists yet has no pool:
// listNVRAM makes one list it.
func sources(h virt.Hypervisor, name string) (cloneSource, error) {
	text, err := h.DomainXML(name, true)
	if err != nil {
		return cloneSource{}, err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return cloneSource{}, err
	}
	disks, err := writableDisks(h, d)
	if err != nil {
		return cloneSource{}, err
	}
	src := cloneSource{domain: d, disks: disks}
	if nv, ok := domxml.NVRAMOf(d); ok && nv.Path != "" {
		pool, vol, err := virt.FindNVRAM(h, nv.Path)
		if err != nil {
			return cloneSource{}, failure.Wrap(failure.KindOf(err), err, "NVRAM %s of %s is in no storage pool", nv.Path, name)
		}
		src.nvram, src.nvramPath, src.nvramFormat = &poolVolume{pool, vol}, nv.Path, nv.Format
	}
	return src, nil
}

// listNVRAM makes a storage pool list the NVRAM when none does yet.
func (src *cloneSource) listNVRAM(h virt.Hypervisor) error {
	if src.nvram == nil || src.nvram.pool != "" {
		return nil
	}
	pool, vol, err := virt.NVRAMVolume(h, src.nvramPath)
	if err != nil {
		return failure.Wrap(failure.KindOf(err), err, "NVRAM %s of %s is in no storage pool", src.nvramPath, src.domain.Name)
	}
	src.nvram = &poolVolume{pool, vol}
	return nil
}

func defineClone(h virt.Hypervisor, d *libvirtxml.Domain, name string, paths map[string]string, nvram string) error {
	c, err := domxml.Clone(d, name, paths, nvram)
	if err != nil {
		return err
	}
	text, err := c.Marshal()
	if err != nil {
		return err
	}
	_, err = h.DefineDomain(text)
	return err
}

// writableDisks are the disks a clone gets its own copy of. A read-only or
// shareable disk, such as an installer ISO, stays shared.
func writableDisks(h virt.Hypervisor, d *libvirtxml.Domain) ([]domxml.Disk, error) {
	var out []domxml.Disk
	for _, disk := range domxml.Disks(d) {
		if disk.Device != "disk" || disk.Shared {
			continue
		}
		if disk.Pool == "" {
			if disk.Source == "" {
				continue
			}
			pool, vol, err := h.VolumeByPath(disk.Source)
			if err != nil {
				return nil, failure.Wrap(failure.KindOf(err), err, "disk %s is in no storage pool", disk.Source)
			}
			disk.Pool, disk.Volume = pool, vol
		}
		out = append(out, disk)
	}
	return out, nil
}

// copyDisks copies each disk to a new volume next to it and gives the path
// per target, and each volume it created.
func copyDisks(h virt.Hypervisor, disks []domxml.Disk, name string) (map[string]string, []poolVolume, error) {
	paths := map[string]string{}
	var created []poolVolume
	for i, disk := range disks {
		vol := name
		if i > 0 {
			vol = fmt.Sprintf("%s-%d", name, i)
		}
		format := disk.Format
		if format == "" {
			format = "raw"
		}
		if format == "qcow2" {
			vol += ".qcow2"
		} else {
			vol += ".img"
		}
		info, err := h.VolumeInfo(disk.Pool, disk.Volume)
		if err != nil {
			return nil, created, err
		}
		v, err := h.CreateVolumeFrom(disk.Pool, domxml.Volume(vol, info.Capacity, format), disk.Pool, disk.Volume)
		if err != nil {
			return nil, created, err
		}
		created = append(created, poolVolume{disk.Pool, vol})
		paths[disk.Target] = v.Path
	}
	return paths, created, nil
}

// copyNVRAM copies the NVRAM of a UEFI domain to <name>_VARS next to it,
// and adds the copy to created. libvirt's firmware autoselection refuses an
// NVRAM template in qcow2 format, so the clone gets a file of its own.
func copyNVRAM(h virt.Hypervisor, src cloneSource, name string, created []poolVolume) (string, []poolVolume, error) {
	if src.nvram == nil {
		return "", created, nil
	}
	format := src.nvramFormat
	if format == "" {
		format = "raw"
	}
	copyName := name + "_VARS.fd"
	if format == "qcow2" {
		copyName = name + "_VARS.qcow2"
	}
	v, err := h.CreateVolumeFrom(src.nvram.pool, domxml.Volume(copyName, 0, format), src.nvram.pool, src.nvram.name)
	if err != nil {
		return "", created, err
	}
	return v.Path, append(created, poolVolume{src.nvram.pool, copyName}), nil
}

// removeClone undefines the shut-off clone with its reset points, and
// deletes its own disk and NVRAM volumes and the NVRAM copies vm made for
// its reset points. An NVRAM in a pool goes through the storage API: an
// undefine that deletes it unlinks the file behind the back of the storage
// driver, whose pool then still holds the name, and the copy of the new
// clone's NVRAM under that name fails. Only an NVRAM no pool can list goes
// with the undefine; a pool error stops the removal.
func removeClone(h virt.Hypervisor, name string) error {
	text, err := h.DomainXML(name, true)
	if err != nil {
		return err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return err
	}
	disks, err := writableDisks(h, d)
	if err != nil {
		return err
	}
	var volumes []poolVolume
	for _, disk := range disks {
		volumes = append(volumes, poolVolume{disk.Pool, disk.Volume})
	}
	flags := virt.UndefineSnapshots
	if nv, ok := domxml.NVRAMOf(d); ok {
		pool, vol, err := virt.NVRAMVolume(h, nv.Path)
		switch {
		case err == nil:
			flags |= virt.UndefineKeepNVRAM
			backups, err := nvramBackups(h, pool, vol)
			if err != nil {
				return err
			}
			volumes = append(append(volumes, poolVolume{pool, vol}), backups...)
		case failure.Is(err, failure.NotFound):
			flags |= virt.UndefineNVRAM
		default:
			return err
		}
	}
	if err := h.UndefineDomain(name, flags); err != nil {
		return err
	}
	for _, v := range volumes {
		if err := h.DeleteVolume(v.pool, v.name); err != nil && !failure.Is(err, failure.NotFound) {
			return err
		}
	}
	return nil
}

// nvramBackups is the copies of the NVRAM volume nvram that vm made in pool
// for its reset points. vm wrote them outside the pool API, so the pool
// lists them only after a refresh.
func nvramBackups(h virt.Hypervisor, pool, nvram string) ([]poolVolume, error) {
	if err := h.RefreshPool(pool); err != nil {
		return nil, err
	}
	names, err := h.Volumes(pool)
	if err != nil {
		return nil, err
	}
	var out []poolVolume
	for _, name := range resetpoint.Backups(nvram, names) {
		out = append(out, poolVolume{pool, name})
	}
	return out, nil
}

// Generalize boots the clone and gives it its own machine-id, SSH host keys
// and hostname, so it runs next to the build VM without a DHCP or
// known_hosts clash, then powers it off.
type Generalize struct{}

func (Generalize) Check(r *pipeline.Run) error {
	declarePassword(r)
	declareDiskPassword(r)
	return nil
}

func (Generalize) Run(r *pipeline.Run) error {
	r.Printf("Giving %s its own machine-id, SSH host keys and hostname", r.Domain)
	if err := boot(r); err != nil {
		return err
	}
	if err := waitSSH(r); err != nil {
		return err
	}
	if err := r.Root(assets.Generalize, r.Domain); err != nil {
		return failure.Wrap(failure.Failed, err, "could not generalize %s", r.Domain)
	}
	return poweroff(r)
}

// Snapshot takes a reset point of the shut-off domain. On a build VM that
// has the label already, Check asks before it is retaken.
type Snapshot struct {
	Label string
	// OnClone is set when a clone step comes before, so the domain does not
	// exist at check time and has no reset points. The clone inherits the
	// disk and NVRAM formats of the build VM, so the guard runs on that.
	OnClone bool
	replace bool
}

func (s *Snapshot) Check(r *pipeline.Run) error {
	if s.OnClone {
		return resetpoint.Guard(r.H, r.BuildVM)
	}
	if err := resetpoint.Guard(r.H, r.Domain); err != nil {
		return err
	}
	exists, err := resetpoint.Has(r.H, r.Domain, s.Label)
	if err != nil || !exists {
		return err
	}
	s.replace = true
	r.Ask(fmt.Sprintf("%s already has a '%s'. It will be retaken from %s as it is now.", r.Domain, s.Label, r.Domain))
	return nil
}

func (s *Snapshot) Run(r *pipeline.Run) error {
	r.Printf("Taking the reset point '%s' of %s", s.Label, r.Domain)
	return resetpoint.Take(r.H, r.Domain, s.Label, s.replace)
}
