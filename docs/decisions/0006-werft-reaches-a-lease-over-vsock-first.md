# 0006: werft reaches a lease over AF_VSOCK first, and over its DHCP lease by MAC as fallback

A vsock CID is unique per domain and has no DNS race. The DHCP fallback serves a guest with a
systemd older than 256, which has no vsock SSH. The seal boot of a systemd 255 guest uses the same
DHCP fallback.

Rejected: mDNS names for leases, because a name lookup can race the guest.
