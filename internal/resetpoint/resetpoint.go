// Package resetpoint takes and restores the reset points of a build VM:
// native libvirt snapshots of the shut-off domain, disk and NVRAM together
// (docs/DESIGN.md, "Reset points").
package resetpoint

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
)

// Labels and names that carry over from vm.
const (
	CleanInstall  = "clean-install"
	ProvisionBase = "provision-base"
	// Clean is the reset point provision takes on the -full clone.
	Clean      = "clean"
	FullSuffix = "-full"
)

// MinLibvirt is the first libvirt that snapshots a qcow2 NVRAM with the disk.
var MinLibvirt = virt.Version{Major: 11, Minor: 4}

// Guard refuses a domain whose reset point would miss a half: a UEFI NVRAM
// that is not qcow2, a disk that is not qcow2, or a libvirt that leaves the
// NVRAM out.
func Guard(h virt.Hypervisor, domain string) error {
	v, err := h.LibVersion()
	if err != nil {
		return err
	}
	if !v.AtLeast(MinLibvirt) {
		return failure.New(failure.Unsupported, "libvirt %s cannot snapshot the NVRAM; reset points need %s or later", v, MinLibvirt)
	}
	text, err := h.DomainXML(domain, true)
	if err != nil {
		return err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return err
	}
	if nv, ok := domxml.NVRAMOf(d); domxml.UEFI(d) && (!ok || nv.Format != "qcow2") {
		return failure.New(failure.Unsupported, "%s keeps its UEFI variables in raw format, which a reset point leaves out; run werft migrate %s", domain, domain)
	}
	for _, disk := range domxml.Disks(d) {
		if disk.Device == "disk" && !disk.Shared && disk.Format != "qcow2" {
			return failure.New(failure.Unsupported, "disk %s of %s is %s, not qcow2; run werft migrate %s", disk.Target, domain, disk.Format, domain)
		}
	}
	return nil
}

// Legacy is the labels of the reset points vm keeps for domain: a copy of
// the NVRAM named <nvram>.<label>.bak next to it. It lists the directory, as
// vm did, and not the storage pool: libvirt refreshes a pool only on request,
// and vm writes the copies outside the pool API. The nvram directory of
// libvirt is world-readable, so no root is needed. A domain without NVRAM,
// or with an NVRAM in a directory werft cannot list, has none.
func Legacy(h virt.Hypervisor, domain string) ([]string, error) {
	text, err := h.DomainXML(domain, true)
	if err != nil {
		return nil, err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return nil, err
	}
	nv, ok := domxml.NVRAMOf(d)
	if !ok || nv.Path == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(filepath.Dir(nv.Path))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return nil, nil
	}
	if err != nil {
		return nil, failure.Wrap(failure.Failed, err, "list the NVRAM directory of %s", domain)
	}
	base := filepath.Base(nv.Path)
	var labels []string
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), base+".")
		if label, ok2 := strings.CutSuffix(rest, ".bak"); ok && ok2 && label != "" {
			labels = append(labels, label)
		}
	}
	return labels, nil
}

func Has(h virt.Hypervisor, domain, label string) (bool, error) {
	names, err := h.Snapshots(domain)
	return slices.Contains(names, label), err
}

// Take saves the shut-off domain as label. With replace an existing label is
// deleted first, since libvirt cannot overwrite one; without it an existing
// label is a conflict.
func Take(h virt.Hypervisor, domain, label string, replace bool) error {
	if err := requireShutOff(h, domain, "a reset point"); err != nil {
		return err
	}
	if err := Guard(h, domain); err != nil {
		return err
	}
	exists, err := Has(h, domain, label)
	if err != nil {
		return err
	}
	if exists {
		if !replace {
			return failure.New(failure.Conflict, "%s already has a reset point '%s'; werft snapshots %s lists them, --replace retakes it", domain, label, domain)
		}
		if err := h.DeleteSnapshot(domain, label); err != nil {
			return err
		}
	}
	return h.CreateSnapshot(domain, label, "")
}

// Revert restores the shut-off domain to label.
func Revert(h virt.Hypervisor, domain, label string) error {
	if err := CheckRevert(h, domain, label); err != nil {
		return err
	}
	return h.RevertSnapshot(domain, label)
}

// CheckRevert fails unless Revert can restore the domain to label now.
func CheckRevert(h virt.Hypervisor, domain, label string) error {
	if err := requireShutOff(h, domain, "a reset"); err != nil {
		return err
	}
	if err := Guard(h, domain); err != nil {
		return err
	}
	exists, err := Has(h, domain, label)
	if err != nil {
		return err
	}
	if !exists {
		return failure.New(failure.NotFound, "%s has no reset point '%s'; werft snapshots %s lists them", domain, label, domain)
	}
	return nil
}

func requireShutOff(h virt.Hypervisor, domain, what string) error {
	s, err := h.DomainState(domain)
	if failure.Is(err, failure.NotFound) {
		return failure.New(failure.NotFound, "unknown VM %s; werft list shows the VMs", domain)
	}
	if err != nil {
		return err
	}
	if s != virt.StateShutOff {
		return failure.New(failure.Conflict, "%s is %s; %s needs it shut off", domain, s, what)
	}
	return nil
}
