package domxml

import (
	"fmt"

	"libvirt.org/go/libvirtxml"
)

// Lease is the definition of the lease name from tmpl, the definition of
// its stock image. The disk on the file stockDisk moves to the overlay file;
// every other disk goes. libvirt fills the NVRAM of the lease from the qcow2
// stock vars at the first start, and deletes it with the domain. The lease
// gets a new UUID and MAC, an auto vsock CID, creds as its only credentials,
// and metadata as its only metadata. Firmware autoselection refuses a qcow2
// template, so the lease keeps the explicit loader of tmpl, which it must
// have.
func Lease(tmpl *libvirtxml.Domain, name, stockDisk, overlay, vars, metadata string, creds []Credential) (*libvirtxml.Domain, error) {
	if tmpl.OS == nil || tmpl.OS.Loader == nil || tmpl.OS.Loader.Path == "" {
		return nil, fmt.Errorf("the stock definition of %s names no loader file", name)
	}
	var target string
	for _, disk := range Disks(tmpl) {
		if disk.Source == stockDisk {
			target = disk.Target
		}
	}
	if target == "" {
		return nil, fmt.Errorf("the stock definition of %s has no disk on %s", name, stockDisk)
	}
	d, err := Clone(tmpl, name, map[string]string{target: overlay}, "")
	if err != nil {
		return nil, err
	}
	var disks []libvirtxml.DomainDisk
	for _, disk := range d.Devices.Disks {
		if disk.Target != nil && disk.Target.Dev == target {
			disks = append(disks, disk)
		}
	}
	d.Devices.Disks = disks
	d.OS.Firmware, d.OS.FirmwareInfo = "", nil
	d.OS.NVRam = &libvirtxml.DomainNVRam{Template: vars, TemplateFormat: "qcow2", Format: "qcow2"}
	d.Title, d.Description = "", ""
	d.Metadata = &libvirtxml.DomainMetadata{XML: metadata}
	EnsureVsock(d)
	SetCredentials(d, creds)
	return d, nil
}

// Overlay is the XML of a new qcow2 volume of capacity bytes on the qcow2
// file backing.
func Overlay(name string, capacity uint64, backing string) string {
	v := libvirtxml.StorageVolume{
		Name:     name,
		Capacity: &libvirtxml.StorageVolumeSize{Unit: "bytes", Value: capacity},
		Target:   &libvirtxml.StorageVolumeTarget{Format: &libvirtxml.StorageVolumeTargetFormat{Type: "qcow2"}},
		BackingStore: &libvirtxml.StorageVolumeBackingStore{
			Path:   backing,
			Format: &libvirtxml.StorageVolumeTargetFormat{Type: "qcow2"},
		},
	}
	text, _ := v.Marshal() // strings and a number always marshal
	return text
}
