package domxml

import (
	"crypto/rand"
	"fmt"

	"libvirt.org/go/libvirtxml"
)

// Clone is the definition of a copy of src named name. It has no UUID, so
// libvirt gives it a new one, a new random MAC per interface, and an auto
// vsock CID. diskPaths maps a disk target to the file of its copy; a disk not
// in it keeps its source, as a shared installer ISO does. nvramPath is the
// file of the NVRAM copy, in the format of the original.
func Clone(src *libvirtxml.Domain, name string, diskPaths map[string]string, nvramPath string) (*libvirtxml.Domain, error) {
	text, err := src.Marshal()
	if err != nil {
		return nil, fmt.Errorf("copy domain XML: %w", err)
	}
	d, err := Parse(text)
	if err != nil {
		return nil, err
	}
	d.Name, d.UUID = name, ""
	if d.OS != nil && d.OS.NVRam != nil {
		nvram, _ := NVRAMOf(src)
		d.OS.NVRam = &libvirtxml.DomainNVRam{NVRam: nvramPath, Format: nvram.Format}
	}
	if d.Devices == nil {
		return d, nil
	}
	for i := range d.Devices.Disks {
		disk := &d.Devices.Disks[i]
		if disk.Target == nil {
			continue
		}
		if path, ok := diskPaths[disk.Target.Dev]; ok {
			disk.Source = &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: path}}
		}
	}
	for i := range d.Devices.Interfaces {
		if iface := &d.Devices.Interfaces[i]; iface.MAC != nil {
			iface.MAC.Address = RandomMAC()
		}
	}
	if v := d.Devices.VSock; v != nil && v.CID != nil {
		v.CID = &libvirtxml.DomainVSockCID{Auto: "yes"}
	}
	return d, nil
}

// RandomMAC is a MAC under the 52:54:00 prefix QEMU and libvirt use.
func RandomMAC() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
}
