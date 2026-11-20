# 0003: A reset point is a native libvirt internal snapshot that holds disk and NVRAM

The NVRAM is in qcow2 format, so libvirt reverts disk and firmware in one atomic operation, without
sudo.

Rejected: the `qemu-img` snapshots plus `.bak` NVRAM copies of `vm`, the bash tool that werft
replaced, because disk and firmware then revert in two operations, and the copies need sudo.
