package virt

import (
	"net"
	"strconv"

	"github.com/hk9890/werft/internal/domxml"
)

// DomainAddresses are the ways to reach the running domain: vsock/<cid> when
// it has a vsock CID, and the IPv4 DHCP leases of its MACs on libvirt
// networks. Both are empty when the domain does not run or has none.
func DomainAddresses(h Hypervisor, name string) (cid string, ips []string) {
	text, err := h.DomainXML(name, false)
	if err != nil {
		return "", nil
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return "", nil
	}
	if c, ok := domxml.VsockCID(d); ok {
		cid = "vsock/" + strconv.FormatUint(uint64(c), 10)
	}
	for _, i := range domxml.Interfaces(d) {
		if i.Network == "" || i.MAC == "" {
			continue
		}
		leases, _ := h.DHCPLeases(i.Network, i.MAC)
		for _, l := range leases {
			if net.ParseIP(l.IP).To4() != nil {
				ips = append(ips, l.IP)
			}
		}
	}
	return cid, ips
}
