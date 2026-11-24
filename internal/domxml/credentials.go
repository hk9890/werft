package domxml

import (
	"encoding/base64"

	"libvirt.org/go/libvirtxml"
)

// Credential is a systemd credential the guest reads from an SMBIOS OEM
// string. fw_cfg is no option: QEMU limits its names to 55 characters.
type Credential struct {
	Name  string
	Value []byte
}

// SetCredentials makes the SMBIOS OEM strings of d carry creds and nothing
// else, so no credential of the domain d was copied from reaches the guest.
func SetCredentials(d *libvirtxml.Domain, creds []Credential) {
	entries := make([]string, len(creds))
	for i, c := range creds {
		entries[i] = "io.systemd.credential.binary:" + c.Name + "=" + base64.StdEncoding.EncodeToString(c.Value)
	}
	if d.OS == nil {
		d.OS = &libvirtxml.DomainOS{}
	}
	d.OS.SMBios = &libvirtxml.DomainSMBios{Mode: "sysinfo"}
	d.SysInfo = []libvirtxml.DomainSysInfo{{SMBIOS: &libvirtxml.DomainSysInfoSMBIOS{
		OEMStrings: &libvirtxml.DomainSysInfoOEMStrings{Entry: entries},
	}}}
}

// EnsureVsock gives d a virtio vsock device with an auto CID, unless it has
// one.
func EnsureVsock(d *libvirtxml.Domain) {
	if d.Devices == nil {
		d.Devices = &libvirtxml.DomainDeviceList{}
	}
	if d.Devices.VSock == nil {
		d.Devices.VSock = &libvirtxml.DomainVSock{Model: "virtio", CID: &libvirtxml.DomainVSockCID{Auto: "yes"}}
	}
}

// RemoveVsock takes the vsock device off d, so the address rules of a guest
// without SSH over AF_VSOCK return its DHCP address.
func RemoveVsock(d *libvirtxml.Domain) {
	if d.Devices != nil {
		d.Devices.VSock = nil
	}
}
