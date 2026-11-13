// Package domxml reads the parts of a libvirt domain XML that werft acts on,
// and builds the domain and volume XML werft defines.
package domxml

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

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
	// Shared is a read-only or shareable disk, such as an installer ISO.
	Shared bool
}

func Disks(d *libvirtxml.Domain) []Disk {
	if d.Devices == nil {
		return nil
	}
	var out []Disk
	for _, x := range d.Devices.Disks {
		disk := Disk{Device: x.Device, Shared: x.ReadOnly != nil || x.Shareable != nil}
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

// Interface is a network interface: its MAC and, for one on a libvirt
// network, the network name.
type Interface struct {
	MAC     string
	Network string
}

func Interfaces(d *libvirtxml.Domain) []Interface {
	if d.Devices == nil {
		return nil
	}
	var out []Interface
	for _, i := range d.Devices.Interfaces {
		var x Interface
		if i.MAC != nil {
			x.MAC = i.MAC.Address
		}
		if i.Source != nil && i.Source.Network != nil {
			x.Network = i.Source.Network.Network
		}
		out = append(out, x)
	}
	return out
}

func Description(d *libvirtxml.Domain) string { return d.Description }

// LUKSMarker is the line in a domain description that says the disk asks
// for the guest password at boot. vm wrote the same line, so a migrated
// domain keeps it. The description is libvirt's, not the disk's: a reset
// keeps it, and a clone copies it.
const LUKSMarker = "vm: the disk asks for the guest password at boot"

func HasLUKSMarker(description string) bool {
	return slices.Contains(strings.Split(description, "\n"), LUKSMarker)
}

// systemdPrefix starts the line in a domain description that holds the
// systemd version of the guest, as detect-guest found it. A reset point
// holds the description of its time, so it tells the version of its guest.
const systemdPrefix = "werft: the guest has systemd "

func SystemdLine(version int) string { return systemdPrefix + strconv.Itoa(version) }

func IsSystemdLine(line string) bool { return strings.HasPrefix(line, systemdPrefix) }

// SystemdOf is the systemd version in the description; false when no line
// holds one.
func SystemdOf(description string) (int, bool) {
	for _, line := range strings.Split(description, "\n") {
		if v, ok := strings.CutPrefix(line, systemdPrefix); ok {
			n, err := strconv.Atoi(v)
			return n, err == nil
		}
	}
	return 0, false
}

// QCOW2NVRAM makes d keep its UEFI variables in the qcow2 file path, so a
// libvirt snapshot holds them. Firmware autoselection knows only raw NVRAM,
// so d gets its loader as an explicit path, which it must have.
func QCOW2NVRAM(d *libvirtxml.Domain, path string) error {
	if d.OS == nil || d.OS.Loader == nil || d.OS.Loader.Path == "" {
		return fmt.Errorf("domain %s names no loader file to keep when firmware autoselection goes", d.Name)
	}
	d.OS.Firmware, d.OS.FirmwareInfo = "", nil
	if d.OS.Loader.Format == "" {
		d.OS.Loader.Format = "raw"
	}
	d.OS.NVRam = &libvirtxml.DomainNVRam{NVRam: path, Format: "qcow2"}
	return nil
}

// Memory is the memory of d in bytes. libvirt reads a size without a unit
// as KiB.
func Memory(d *libvirtxml.Domain) (uint64, error) {
	if d.Memory == nil {
		return 0, fmt.Errorf("domain %s names no memory size", d.Name)
	}
	n := uint64(d.Memory.Value)
	switch d.Memory.Unit {
	case "b", "bytes":
		return n, nil
	case "", "k", "KiB":
		return n << 10, nil
	case "KB":
		return n * 1e3, nil
	case "M", "MiB":
		return n << 20, nil
	case "MB":
		return n * 1e6, nil
	case "G", "GiB":
		return n << 30, nil
	case "GB":
		return n * 1e9, nil
	case "T", "TiB":
		return n << 40, nil
	case "TB":
		return n * 1e12, nil
	}
	return 0, fmt.Errorf("domain %s gives its memory in the unknown unit %q", d.Name, d.Memory.Unit)
}
