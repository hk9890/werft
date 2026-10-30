package resetpoint

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
)

// Migration moves a build VM from the reset points of vm to native ones
// (docs/DESIGN.md, "One-time migration"). A reset point of vm is a qemu-img
// snapshot in the first disk plus a copy of the raw NVRAM named
// <nvram>.<label>.bak. werft reaches both without root: libvirt takes the
// qemu-img snapshot as its own when werft redefines its metadata, and the
// copies are volumes in the pool of the NVRAM.
//
// Every step can run again, so a migration that stopped half way finishes
// when it runs again.
type Migration struct {
	Domain string
	// Keep is the label that becomes a native reset point: KeepLabel, or
	// clean-install when vm has no KeepLabel. Empty when vm has neither:
	// then the domain keeps its current disk and NVRAM, and gets no reset
	// point.
	Keep string
	// Delete is the other labels of vm, which go away.
	Delete []string
	// Export is the volumes in ExportPool that keep the clean-install point
	// of vm, disk and then NVRAM, as flat files. Empty when there is none.
	ExportPool string
	Export     []string
	// Resume is set when an earlier migration stopped after the domain got
	// its qcow2 NVRAM; only the last step is left.
	Resume bool

	def       *libvirtxml.Domain
	disk      poolVolume
	disks     []string
	nvramPool string
	rawNVRAM  string
}

type poolVolume struct{ pool, name string }

// KeepLabel is the reset point of vm that migrate keeps: the base that
// provision starts from, or the result of provision on a -full clone.
func KeepLabel(domain string) string {
	if strings.HasSuffix(domain, FullSuffix) {
		return Clean
	}
	return ProvisionBase
}

// PlanMigration checks that the shut-off domain can move to native reset
// points and finds what moves. It changes nothing but a refresh of the
// NVRAM pool, which makes libvirt list the copies vm wrote.
func PlanMigration(h virt.Hypervisor, domain, exportPool string) (*Migration, error) {
	if err := requireShutOff(h, domain, "a migration"); err != nil {
		return nil, err
	}
	v, err := h.LibVersion()
	if err != nil {
		return nil, err
	}
	if !v.AtLeast(MinLibvirt) {
		return nil, failure.New(failure.Unsupported, "libvirt %s cannot snapshot the NVRAM; native reset points need %s or later", v, MinLibvirt)
	}
	text, err := h.DomainXML(domain, true)
	if err != nil {
		return nil, err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return nil, err
	}
	m := &Migration{Domain: domain, Keep: KeepLabel(domain), ExportPool: exportPool, def: d}
	nv, ok := domxml.NVRAMOf(d)
	if !domxml.UEFI(d) || !ok || nv.Path == "" {
		return nil, failure.New(failure.Unsupported, "%s has no UEFI NVRAM; werft migrate finds the reset points of vm only by their NVRAM copies", domain)
	}
	if err := m.findDisks(h); err != nil {
		return nil, err
	}
	if nv.Format == "qcow2" {
		return m, m.planResume(h, nv.Path)
	}
	if l, _ := domxml.LoaderOf(d); l.Path == "" {
		return nil, failure.New(failure.Unsupported, "%s names no loader file; a qcow2 NVRAM needs one, because firmware autoselection knows only raw NVRAM", domain)
	}
	if m.nvramPool, m.rawNVRAM, err = h.VolumeByPath(nv.Path); err != nil {
		return nil, failure.Wrap(failure.Unsupported, err, "the NVRAM %s of %s is in no storage pool, and werft changes files only through one", nv.Path, domain)
	}
	if err := h.RefreshPool(m.nvramPool); err != nil {
		return nil, err
	}
	labels, err := m.legacyLabels(h)
	if err != nil {
		return nil, err
	}
	switch {
	case slices.Contains(labels, m.Keep):
		if slices.Contains(labels, CleanInstall) {
			m.Export = []string{domain + "-" + CleanInstall + ".qcow2", domain + "-" + CleanInstall + "_VARS.qcow2"}
		}
	case slices.Contains(labels, CleanInstall):
		m.Keep = CleanInstall
	default:
		m.Keep = ""
	}
	for _, label := range labels {
		if label != m.Keep && label != CleanInstall {
			m.Delete = append(m.Delete, label)
		}
	}
	return m, m.refuseForeignExport(h)
}

// refuseForeignExport refuses an export volume that no earlier run of this
// migration made. An earlier run redefines clean-install before it copies,
// and deletes that metadata only after it deleted the NVRAM copy.
func (m *Migration) refuseForeignExport(h virt.Hypervisor) error {
	if len(m.Export) == 0 {
		return nil
	}
	if ok, err := Has(h, m.Domain, CleanInstall); err != nil || ok {
		return err
	}
	for _, name := range m.Export {
		if ok, err := hasVolume(h, m.ExportPool, name); err != nil || ok {
			return errOr(err, failure.New(failure.Conflict, "volume %s in pool %s is not from a migration of this %s; delete or move it: virsh -c qemu:///system vol-delete --pool %s %s", name, m.ExportPool, m.Domain, m.ExportPool, name))
		}
	}
	return nil
}

// findDisks finds the writable disks. vm took its qemu-img snapshots of the
// first one only.
func (m *Migration) findDisks(h virt.Hypervisor) error {
	for _, disk := range domxml.Disks(m.def) {
		if disk.Device != "disk" || disk.Shared {
			continue
		}
		if disk.Format != "qcow2" {
			return failure.New(failure.Unsupported, "disk %s of %s is %s, not qcow2, and cannot hold a reset point", disk.Target, m.Domain, disk.Format)
		}
		m.disks = append(m.disks, disk.Target)
		if len(m.disks) > 1 {
			continue
		}
		pool, name := disk.Pool, disk.Volume
		if name == "" {
			var err error
			if pool, name, err = h.VolumeByPath(disk.Source); err != nil {
				return failure.Wrap(failure.Unsupported, err, "disk %s of %s is in no storage pool, and werft copies disks only through one", disk.Source, m.Domain)
			}
		}
		m.disk = poolVolume{pool, name}
	}
	if len(m.disks) == 0 {
		return failure.New(failure.Unsupported, "%s has no writable disk", m.Domain)
	}
	return nil
}

// planResume finds a migration that stopped after the domain got its qcow2
// NVRAM: the kept label is still the snapshot of vm, whose domain has the
// raw NVRAM.
func (m *Migration) planResume(h virt.Hypervisor, nvram string) error {
	for _, label := range []string{m.Keep, CleanInstall} {
		raw, err := m.legacyNVRAM(h, label)
		if err != nil {
			return err
		}
		if raw == "" {
			continue
		}
		if m.nvramPool, _, err = h.VolumeByPath(nvram); err != nil {
			return err
		}
		m.Keep, m.Resume, m.rawNVRAM = label, true, filepath.Base(raw)
		return nil
	}
	return failure.New(failure.Conflict, "%s keeps its UEFI variables in qcow2 and has no reset points of vm; nothing to migrate", m.Domain)
}

// legacyNVRAM is the path of the raw NVRAM that the snapshot label names
// when it is a reset point of vm, else empty.
func (m *Migration) legacyNVRAM(h virt.Hypervisor, label string) (string, error) {
	if ok, err := Has(h, m.Domain, label); err != nil || !ok {
		return "", err
	}
	text, err := h.SnapshotXML(m.Domain, label)
	if err != nil {
		return "", err
	}
	var s libvirtxml.DomainSnapshot
	if err := s.Unmarshal(text); err != nil {
		return "", failure.Wrap(failure.Failed, err, "snapshot %s of %s", label, m.Domain)
	}
	if s.Domain == nil {
		return "", nil
	}
	if raw, ok := domxml.NVRAMOf(s.Domain); ok && raw.Format != "qcow2" {
		return raw.Path, nil
	}
	return "", nil
}

func errOr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

// legacyLabels is the label of each NVRAM copy of vm, in the order of the
// pool listing, then each label an earlier run redefined and did not finish
// to delete.
func (m *Migration) legacyLabels(h virt.Hypervisor) ([]string, error) {
	names, err := h.Volumes(m.nvramPool)
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, name := range names {
		rest, ok := strings.CutPrefix(name, m.rawNVRAM+".")
		if label, ok2 := strings.CutSuffix(rest, ".bak"); ok && ok2 && label != "" {
			labels = append(labels, label)
		}
	}
	snapshots, err := h.Snapshots(m.Domain)
	if err != nil {
		return nil, err
	}
	for _, name := range snapshots {
		if slices.Contains(labels, name) {
			continue
		}
		text, err := h.SnapshotXML(m.Domain, name)
		if err != nil {
			return nil, err
		}
		var s libvirtxml.DomainSnapshot
		if err := s.Unmarshal(text); err != nil {
			return nil, failure.Wrap(failure.Failed, err, "snapshot %s of %s", name, m.Domain)
		}
		if s.Description == legacyDescription {
			labels = append(labels, name)
		}
	}
	return labels, nil
}

func (m *Migration) backup(label string) string { return m.rawNVRAM + "." + label + ".bak" }

// QCOW2NVRAM is the volume of the new NVRAM, next to the raw one.
func (m *Migration) QCOW2NVRAM() string { return m.Domain + "_VARS.qcow2" }

// Run carries out the migration. progress gets a line per step.
func (m *Migration) Run(h virt.Hypervisor, progress func(format string, args ...any)) error {
	if !m.Resume {
		if m.Keep != "" {
			progress("Checking that the disk of %s has the reset point '%s'", m.Domain, m.Keep)
			if err := m.revert(h, m.Keep); err != nil {
				return err
			}
		}
		if len(m.Export) > 0 {
			progress("Exporting the reset point '%s' of %s to %s in pool %s", CleanInstall, m.Domain, strings.Join(m.Export, " and "), m.ExportPool)
			if err := m.export(h); err != nil {
				return err
			}
		}
		for _, label := range m.Delete {
			progress("Deleting the reset point '%s' of vm", label)
			if err := m.drop(h, label); err != nil {
				return err
			}
		}
		progress("Moving the UEFI variables of %s to %s", m.Domain, m.QCOW2NVRAM())
		if err := m.convert(h); err != nil {
			return err
		}
	}
	if m.Keep != "" {
		progress("Taking '%s' as a native reset point", m.Keep)
	}
	return m.finish(h)
}

// export copies the disk at clean-install and its NVRAM copy to flat qcow2
// volumes, then deletes the reset point. A volume that exists already is
// from an earlier run (refuseForeignExport): libvirt deletes a copy that
// fails.
func (m *Migration) export(h virt.Hypervisor) error {
	disk, vars := m.Export[0], m.Export[1]
	if ok, err := hasVolume(h, m.ExportPool, disk); err != nil {
		return err
	} else if !ok {
		if err := m.revert(h, CleanInstall); err != nil {
			return err
		}
		info, err := h.VolumeInfo(m.disk.pool, m.disk.name)
		if err != nil {
			return err
		}
		if _, err := h.CreateVolumeFrom(m.ExportPool, qcow2Volume(disk, info.Capacity), m.disk.pool, m.disk.name); err != nil {
			return err
		}
	}
	if ok, err := hasVolume(h, m.ExportPool, vars); err != nil {
		return err
	} else if !ok {
		if _, err := h.CreateVolumeFrom(m.ExportPool, qcow2Volume(vars, 0), m.nvramPool, m.backup(CleanInstall)); err != nil {
			return err
		}
	}
	return m.drop(h, CleanInstall)
}

func hasVolume(h virt.Hypervisor, pool, name string) (bool, error) {
	_, err := h.VolumePath(pool, name)
	if failure.Is(err, failure.NotFound) {
		return false, nil
	}
	return err == nil, err
}

func qcow2Volume(name string, capacity uint64) string {
	return fmt.Sprintf("<volume><name>%s</name><capacity unit='bytes'>%d</capacity><target><format type='qcow2'/></target></volume>", name, capacity)
}

const legacyDescription = "reset point of vm, found by werft migrate"

// legacy makes libvirt know the qemu-img snapshot label of vm, so libvirt
// reverts and deletes it. libvirt checks neither that the disk has it nor
// what it holds.
func (m *Migration) legacy(h virt.Hypervisor, label string) error {
	if ok, err := Has(h, m.Domain, label); err != nil || ok {
		return err
	}
	disks := make([]libvirtxml.DomainSnapshotDisk, len(m.disks))
	for i, target := range m.disks {
		disks[i] = libvirtxml.DomainSnapshotDisk{Name: target, Snapshot: "no"}
	}
	disks[0].Snapshot = "internal"
	text, err := (&libvirtxml.DomainSnapshot{
		Name:         label,
		Description:  legacyDescription,
		State:        "shutoff",
		CreationTime: strconv.FormatInt(time.Now().Unix(), 10),
		Memory:       &libvirtxml.DomainSnapshotMemory{Snapshot: "no"},
		Disks:        &libvirtxml.DomainSnapshotDisks{Disks: disks},
		Domain:       m.def,
	}).Marshal()
	if err != nil {
		return failure.Wrap(failure.Failed, err, "snapshot XML")
	}
	return h.RedefineSnapshot(m.Domain, text)
}

// revert puts the first disk back to the qemu-img snapshot label of vm.
// It leaves the raw NVRAM as it is.
func (m *Migration) revert(h virt.Hypervisor, label string) error {
	if err := m.legacy(h, label); err != nil {
		return err
	}
	if err := h.RevertSnapshot(m.Domain, label); err != nil {
		return failure.Wrap(failure.KindOf(err), err, "restore the disk of %s to the qemu-img snapshot '%s' of vm; its disk can lack it, though %s exists", m.Domain, label, m.backup(label))
	}
	return nil
}

// drop deletes the NVRAM copy of label, then the qemu-img snapshot of vm.
// The redefined metadata goes last, so a new run still finds the label.
// libvirt ignores a qemu-img snapshot that is gone already.
func (m *Migration) drop(h virt.Hypervisor, label string) error {
	if err := m.legacy(h, label); err != nil {
		return err
	}
	if err := deleteVolume(h, m.nvramPool, m.backup(label)); err != nil {
		return err
	}
	return h.DeleteSnapshot(m.Domain, label)
}

func deleteVolume(h virt.Hypervisor, pool, name string) error {
	if err := h.DeleteVolume(pool, name); err != nil && !failure.Is(err, failure.NotFound) {
		return err
	}
	return nil
}

// convert restores the disk to the kept label, fills a qcow2 NVRAM from its
// copy, and redefines the domain on it. Without a kept label the qcow2 NVRAM
// is a copy of the current one. Until the redefine the domain keeps its raw
// NVRAM, and a new run starts again.
func (m *Migration) convert(h virt.Hypervisor) error {
	from := m.rawNVRAM
	if m.Keep != "" {
		if err := m.revert(h, m.Keep); err != nil {
			return err
		}
		from = m.backup(m.Keep)
	}
	if err := deleteVolume(h, m.nvramPool, m.QCOW2NVRAM()); err != nil {
		return err
	}
	v, err := h.CreateVolumeFrom(m.nvramPool, qcow2Volume(m.QCOW2NVRAM(), 0), m.nvramPool, from)
	if err != nil {
		return err
	}
	if err := m.redefine(h, v.Path); err != nil {
		_ = h.DeleteVolume(m.nvramPool, m.QCOW2NVRAM())
		return failure.Wrap(failure.KindOf(err), err, "%s keeps its raw NVRAM", m.Domain)
	}
	return nil
}

func (m *Migration) redefine(h virt.Hypervisor, nvram string) error {
	text, err := m.def.Marshal()
	if err != nil {
		return err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return err
	}
	if err := domxml.QCOW2NVRAM(d, nvram); err != nil {
		return err
	}
	if text, err = d.Marshal(); err != nil {
		return err
	}
	_, err = h.DefineDomain(text)
	return err
}

// finish deletes the raw NVRAM and the copy of the kept label, then the
// qemu-img snapshot of vm, and takes the native reset point in its place.
// The snapshot of vm goes last: it names the raw NVRAM for a new run.
// Without a kept label nothing names it, so a failed delete names it.
func (m *Migration) finish(h virt.Hypervisor) error {
	if m.Keep == "" {
		if err := deleteVolume(h, m.nvramPool, m.rawNVRAM); err != nil {
			return failure.Wrap(failure.KindOf(err), err, "%s is migrated; delete its old NVRAM with: virsh -c qemu:///system vol-delete --pool %s %s", m.Domain, m.nvramPool, m.rawNVRAM)
		}
		return nil
	}
	for _, name := range []string{m.rawNVRAM, m.backup(m.Keep)} {
		if err := deleteVolume(h, m.nvramPool, name); err != nil {
			return err
		}
	}
	if err := h.DeleteSnapshot(m.Domain, m.Keep); err != nil {
		return err
	}
	if err := Take(h, m.Domain, m.Keep, false); err != nil {
		return failure.Wrap(failure.KindOf(err), err, "%s is migrated but has no reset point '%s'; take it with: werft snapshot %s %s", m.Domain, m.Keep, m.Domain, m.Keep)
	}
	return nil
}
