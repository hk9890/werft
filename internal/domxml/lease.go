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
// and metadata as its only metadata. A serial or console device with a file
// source gets a pty, and every <log> goes: a file of tmpl is one path for
// every lease, and the second lease would fail to open it.
// Firmware autoselection refuses a qcow2 template, so the lease keeps the
// explicit loader of tmpl, which it must have.
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
	for i := range d.Devices.Serials {
		d.Devices.Serials[i].Source, d.Devices.Serials[i].Log = ptyOf(d.Devices.Serials[i].Source), nil
	}
	for i := range d.Devices.Consoles {
		d.Devices.Consoles[i].Source, d.Devices.Consoles[i].Log = ptyOf(d.Devices.Consoles[i].Source), nil
	}
	EnsureVsock(d)
	SetCredentials(d, creds)
	return d, nil
}

// ptyOf is a pty in place of a file source; it keeps any other source.
func ptyOf(s *libvirtxml.DomainChardevSource) *libvirtxml.DomainChardevSource {
	if s == nil || s.File == nil {
		return s
	}
	return &libvirtxml.DomainChardevSource{Pty: &libvirtxml.DomainChardevSourcePty{}}
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

// Resize gives d memory bytes and cpus vCPUs; a zero value keeps that of d.
// A CPU topology becomes one socket with cpus cores. It refuses a d with
// NUMA cells or a maxMemory, whose sizes it would have to split, and new
// vCPUs for a d whose <cputune> names vCPU ids, which could be out of range.
func Resize(d *libvirtxml.Domain, memory uint64, cpus uint) error {
	if memory == 0 && cpus == 0 {
		return nil
	}
	if d.CPU != nil && d.CPU.Numa != nil && len(d.CPU.Numa.Cell) > 0 {
		return fmt.Errorf("domain %s has NUMA cells", d.Name)
	}
	if memory > 0 {
		if d.MaximumMemory != nil {
			return fmt.Errorf("domain %s has a maxMemory for memory hotplug", d.Name)
		}
		kib := uint(memory >> 10)
		d.Memory = &libvirtxml.DomainMemory{Value: kib, Unit: "KiB"}
		if d.CurrentMemory != nil {
			d.CurrentMemory = &libvirtxml.DomainCurrentMemory{Value: kib, Unit: "KiB"}
		}
	}
	if cpus > 0 {
		if t := d.CPUTune; t != nil && len(t.VCPUPin)+len(t.VCPUSched)+len(t.CacheTune)+len(t.MemoryTune) > 0 {
			return fmt.Errorf("domain %s has a <cputune> that names vCPU ids", d.Name)
		}
		vcpu := libvirtxml.DomainVCPU{Value: cpus}
		if d.VCPU != nil {
			vcpu.Placement, vcpu.CPUSet = d.VCPU.Placement, d.VCPU.CPUSet
		}
		d.VCPU, d.VCPUs = &vcpu, nil
		if d.CPU != nil && d.CPU.Topology != nil {
			d.CPU.Topology = &libvirtxml.DomainCPUTopology{Sockets: 1, Cores: int(cpus), Threads: 1}
		}
	}
	return nil
}
