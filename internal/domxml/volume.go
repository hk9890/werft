package domxml

import "libvirt.org/go/libvirtxml"

// Volume is the XML of a new storage volume of capacity bytes in format.
// For a copy, capacity 0 takes the size of the source.
func Volume(name string, capacity uint64, format string) string {
	v := libvirtxml.StorageVolume{
		Name:     name,
		Capacity: &libvirtxml.StorageVolumeSize{Unit: "bytes", Value: capacity},
		Target:   &libvirtxml.StorageVolumeTarget{Format: &libvirtxml.StorageVolumeTargetFormat{Type: format}},
	}
	text, _ := v.Marshal() // strings and a number always marshal
	return text
}
