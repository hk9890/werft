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

func target(r *pipeline.Run) string { return r.BuildVM + FullSuffix }

func (Clone) Check(r *pipeline.Run) error {
	t := target(r)
	state, err := r.H.DomainState(t)
	if failure.Is(err, failure.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != virt.StateShutOff {
		return failure.New(failure.Conflict, "%s is %s; provision replaces it, so it must be shut off first", t, state)
	}
	return r.Confirm(t + " exists and will be replaced. Everything in it is lost.")
}

func (Clone) Run(r *pipeline.Run) error {
	t := target(r)
	if _, err := r.H.Domain(t); err == nil {
		r.Printf("Removing the old %s", t)
		if err := removeClone(r.H, t); err != nil {
			return err
		}
	}
	r.Printf("Cloning %s to %s", r.Domain, t)
	src, err := r.H.Domain(r.Domain)
	if err != nil {
		return err
	}
	text, err := r.H.DomainXML(r.Domain, true)
	if err != nil {
		return err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return err
	}
	paths, created, err := copyDisks(r.H, d, t)
	nvram := ""
	if err == nil {
		nvram, created, err = copyNVRAM(r.H, d, t, created)
	}
	if err == nil {
		err = defineClone(r.H, d, t, paths, nvram)
	}
	if err != nil {
		for _, v := range created {
			_ = r.H.DeleteVolume(v[0], v[1])
		}
		return failure.Wrap(failure.KindOf(err), err, "%s itself is intact", r.Domain)
	}
	dst, err := r.H.Domain(t)
	if err != nil {
		return err
	}
	if err := r.Host.CopyViewerSettings(src.UUID, dst.UUID); err != nil {
		r.Printf("Could not copy virt-manager's viewer settings to %s; its window opens with the defaults", t)
	}
	r.Domain, r.Addr = t, guest.Addr{}
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

// copyDisks copies each writable disk to a new volume next to it and gives
// the path per target, and the pool and name of each volume it created.
func copyDisks(h virt.Hypervisor, d *libvirtxml.Domain, name string) (map[string]string, [][2]string, error) {
	disks, err := writableDisks(h, d)
	if err != nil {
		return nil, nil, err
	}
	paths := map[string]string{}
	var created [][2]string
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
		xml := fmt.Sprintf("<volume><name>%s</name><capacity unit='bytes'>%d</capacity><target><format type='%s'/></target></volume>", vol, info.Capacity, format)
		v, err := h.CreateVolumeFrom(disk.Pool, xml, disk.Pool, disk.Volume)
		if err != nil {
			return nil, created, err
		}
		created = append(created, [2]string{disk.Pool, vol})
		paths[disk.Target] = v.Path
	}
	return paths, created, nil
}

// copyNVRAM copies the NVRAM of a UEFI domain to <name>_VARS next to it,
// and adds the copy to created. libvirt's firmware autoselection refuses an
// NVRAM template in qcow2 format, so the clone gets a file of its own.
func copyNVRAM(h virt.Hypervisor, d *libvirtxml.Domain, name string, created [][2]string) (string, [][2]string, error) {
	nv, ok := domxml.NVRAMOf(d)
	if !ok || nv.Path == "" {
		return "", created, nil
	}
	pool, vol, err := h.VolumeByPath(nv.Path)
	if err != nil {
		return "", created, failure.Wrap(failure.KindOf(err), err, "NVRAM %s is in no storage pool", nv.Path)
	}
	format := nv.Format
	if format == "" {
		format = "raw"
	}
	copyName := name + "_VARS.fd"
	if format == "qcow2" {
		copyName = name + "_VARS.qcow2"
	}
	xml := fmt.Sprintf("<volume><name>%s</name><capacity>0</capacity><target><format type='%s'/></target></volume>", copyName, format)
	v, err := h.CreateVolumeFrom(pool, xml, pool, vol)
	if err != nil {
		return "", created, err
	}
	return v.Path, append(created, [2]string{pool, copyName}), nil
}

// removeClone undefines the shut-off clone with its NVRAM and reset points,
// and deletes its own disk volumes.
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
	flags := virt.UndefineSnapshots
	if _, ok := domxml.NVRAMOf(d); ok {
		flags |= virt.UndefineNVRAM
	}
	if err := h.UndefineDomain(name, flags); err != nil {
		return err
	}
	for _, disk := range disks {
		if err := h.DeleteVolume(disk.Pool, disk.Volume); err != nil && !failure.Is(err, failure.NotFound) {
			return err
		}
	}
	return nil
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
	// exist at check time and has no reset points.
	OnClone bool
	replace bool
}

func (s *Snapshot) Check(r *pipeline.Run) error {
	if s.OnClone {
		return nil
	}
	if err := resetpoint.Guard(r.H, r.Domain); err != nil {
		return err
	}
	exists, err := resetpoint.Has(r.H, r.Domain, s.Label)
	if err != nil || !exists {
		return err
	}
	s.replace = true
	return r.Confirm(fmt.Sprintf("%s already has a '%s'. It will be retaken from %s as it is now.", r.Domain, s.Label, r.Domain))
}

func (s *Snapshot) Run(r *pipeline.Run) error {
	r.Printf("Taking the reset point '%s' of %s", s.Label, r.Domain)
	return resetpoint.Take(r.H, r.Domain, s.Label, s.replace)
}
