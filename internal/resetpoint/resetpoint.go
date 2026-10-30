// Package resetpoint takes and restores the reset points of a build VM:
// native libvirt snapshots of the shut-off domain, disk and NVRAM together
// (docs/DESIGN.md, "Reset points").
package resetpoint

import (
	"slices"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
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
			return failure.New(failure.Conflict, "%s already has a reset point '%s'", domain, label)
		}
		if err := h.DeleteSnapshot(domain, label); err != nil {
			return err
		}
	}
	return h.CreateSnapshot(domain, label, "")
}

// Revert restores the shut-off domain to label.
func Revert(h virt.Hypervisor, domain, label string) error {
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
		return failure.New(failure.NotFound, "%s has no reset point '%s'", domain, label)
	}
	return h.RevertSnapshot(domain, label)
}

func requireShutOff(h virt.Hypervisor, domain, what string) error {
	s, err := h.DomainState(domain)
	if err != nil {
		return err
	}
	if s != virt.StateShutOff {
		return failure.New(failure.Conflict, "%s is %s; %s needs it shut off", domain, s, what)
	}
	return nil
}
