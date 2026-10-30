// Package domxml reads the parts of a libvirt domain XML that werft acts on.
package domxml

import (
	"fmt"
	"strconv"

	"libvirt.org/go/libvirtxml"
)

func Parse(xml string) (*libvirtxml.Domain, error) {
	var d libvirtxml.Domain
	if err := d.Unmarshal(xml); err != nil {
		return nil, fmt.Errorf("parse domain XML: %w", err)
	}
	return &d, nil
}

// Disk is one disk of a domain. Source is the file or block device path;
// Pool and Volume are set instead for a disk given as a pool volume.
type Disk struct {
	Target string
	Device string
	Format string
	Source string
	Pool   string
	Volume string
}

func Disks(d *libvirtxml.Domain) []Disk {
	if d.Devices == nil {
		return nil
	}
	var out []Disk
	for _, x := range d.Devices.Disks {
		disk := Disk{Device: x.Device}
		if x.Target != nil {
			disk.Target = x.Target.Dev
		}
		if x.Driver != nil {
			disk.Format = x.Driver.Type
		}
		if s := x.Source; s != nil {
			switch {
			case s.File != nil:
				disk.Source = s.File.File
			case s.Block != nil:
				disk.Source = s.Block.Dev
			case s.Volume != nil:
				disk.Pool, disk.Volume = s.Volume.Pool, s.Volume.Volume
			}
		}
		out = append(out, disk)
	}
	return out
}

type Loader struct {
	Path     string
	Type     string
	Format   string
	Readonly bool
}

func LoaderOf(d *libvirtxml.Domain) (Loader, bool) {
	if d.OS == nil || d.OS.Loader == nil {
		return Loader{}, false
	}
	l := d.OS.Loader
	return Loader{Path: l.Path, Type: l.Type, Format: l.Format, Readonly: l.Readonly == "yes"}, true
}

type NVRAM struct {
	Path           string
	Format         string
	Template       string
	TemplateFormat string
}

func NVRAMOf(d *libvirtxml.Domain) (NVRAM, bool) {
	if d.OS == nil || d.OS.NVRam == nil {
		return NVRAM{}, false
	}
	n := d.OS.NVRam
	path := n.NVRam
	if path == "" && n.Source != nil && n.Source.File != nil {
		path = n.Source.File.File
	}
	return NVRAM{Path: path, Format: n.Format, Template: n.Template, TemplateFormat: n.TemplateFormat}, true
}

// UEFI reports whether the domain boots UEFI firmware: an EFI firmware
// autoselection, or a pflash loader.
func UEFI(d *libvirtxml.Domain) bool {
	if d.OS == nil {
		return false
	}
	if d.OS.Firmware == "efi" {
		return true
	}
	return d.OS.Loader != nil && d.OS.Loader.Type == "pflash"
}

// VsockCID is the CID libvirt gave the domain's vsock device. Only the live
// XML of a running domain with an auto CID has it.
func VsockCID(d *libvirtxml.Domain) (uint32, bool) {
	if d.Devices == nil || d.Devices.VSock == nil || d.Devices.VSock.CID == nil {
		return 0, false
	}
	cid, err := strconv.ParseUint(d.Devices.VSock.CID.Address, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(cid), true
}

// MACs are the MAC addresses of the domain's network interfaces, in order.
func MACs(d *libvirtxml.Domain) []string {
	if d.Devices == nil {
		return nil
	}
	var out []string
	for _, i := range d.Devices.Interfaces {
		if i.MAC != nil && i.MAC.Address != "" {
			out = append(out, i.MAC.Address)
		}
	}
	return out
}

func Description(d *libvirtxml.Domain) string { return d.Description }
