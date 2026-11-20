# 0002: werft uses only the libvirt URI `qemu:///system`

The NAT network and its DHCP leases exist only on `qemu:///system`. The user is in group
`libvirt`, so the socket needs no sudo and no polkit.

Rejected: `qemu:///session`, because it has no NAT network and no DHCP leases.
