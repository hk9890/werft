package virt

import (
	"net"
	"strconv"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
)

// DomainAddresses are the ways to reach the running domain: vsock/<cid> when
// it has a vsock CID, and the IPv4 DHCP leases of its MACs on libvirt
// networks. Both are empty when the domain does not run or has none.
func DomainAddresses(h Hypervisor, name string) (cid string, ips []string) {
	cid, leases, _ := domainAddresses(h, name)
	for _, l := range leases {
		ips = append(ips, l.IP)
	}
	return cid, ips
}

// DomainLeases are the IPv4 DHCP leases of the domain's MACs; a network
// that does not exist has none. libvirt keeps a lease until it expires, also
// after the domain stopped, so a lease alone does not show that the guest is
// up: compare two reads with NewLeases.
func DomainLeases(h Hypervisor, name string) ([]DHCPLease, error) {
	_, leases, err := domainAddresses(h, name)
	return leases, err
}

// NewLeases are the leases in now the guest took after before was read:
// one with an address not in before, or one that expires later than it did
// there. A lease that never expires has expiry 0 and shows no renewal; it
// counts as new.
func NewLeases(before, now []DHCPLease) []DHCPLease {
	expiry := map[string]int64{}
	for _, l := range before {
		expiry[l.IP] = l.Expiry
	}
	var out []DHCPLease
	for _, l := range now {
		if old, ok := expiry[l.IP]; !ok || l.Expiry > old || l.Expiry == 0 {
			out = append(out, l)
		}
	}
	return out
}

func domainAddresses(h Hypervisor, name string) (cid string, leases []DHCPLease, err error) {
	text, err := h.DomainXML(name, false)
	if err != nil {
		return "", nil, err
	}
	d, err := domxml.Parse(text)
	if err != nil {
		return "", nil, err
	}
	if c, ok := domxml.VsockCID(d); ok {
		cid = "vsock/" + strconv.FormatUint(uint64(c), 10)
	}
	for _, i := range domxml.Interfaces(d) {
		if i.Network == "" || i.MAC == "" {
			continue
		}
		ls, lerr := h.DHCPLeases(i.Network, i.MAC)
		if lerr != nil && !failure.Is(lerr, failure.NotFound) && err == nil {
			err = lerr
		}
		for _, l := range ls {
			if net.ParseIP(l.IP).To4() != nil {
				leases = append(leases, l)
			}
		}
	}
	return cid, leases, err
}
