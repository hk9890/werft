# 0001: werft drives libvirt over its RPC protocol in pure Go, behind one internal interface

werft builds with `CGO_ENABLED=0` and needs typed errors, no text parsing and no process per call:
`github.com/digitalocean/go-libvirt` plus `libvirt.org/go/libvirtxml`. Policy runs against fakes.

Rejected: the cgo bindings `libvirt.org/go/libvirt`, because werft builds without cgo.

Rejected: `virsh` as a child process, because its output is text to parse and each call is a
process. The interface keeps a `virsh` adapter possible, as a fallback for a go-libvirt defect.
