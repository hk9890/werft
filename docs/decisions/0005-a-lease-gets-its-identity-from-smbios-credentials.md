# 0005: A lease gets its identity from systemd credentials over SMBIOS type 11

Nothing per lease is written into a disk image. The identity comes in from outside at boot. A guest
may write its identity to its own lease overlay from a credential; the host never writes it.

Rejected: QEMU fw_cfg, because QEMU limits its names to 55 characters.

Rejected: libguestfs, because it writes the identity into the disk image.
