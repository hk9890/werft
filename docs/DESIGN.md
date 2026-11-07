# werft — design

How werft is built. What werft must do and for whom: [PRODUCT.md](PRODUCT.md). werft replaces the
bash tool `vm` in [`scripts/vm/`](https://github.com/carlo9890/setup/tree/main/scripts/vm) of the
setup repo.

Status of each phase: [PRODUCT.md, "Roadmap"](PRODUCT.md#roadmap).

## Proposals in short

Each row is a proposal until the user accepts it.

| Topic | Proposal | Why |
|---|---|---|
| libvirt access | `github.com/digitalocean/go-libvirt` (pure Go RPC) plus `libvirt.org/go/libvirtxml`, behind one internal interface | Works with `CGO_ENABLED=0`. Typed errors, no text parsing, no process per call. |
| URI | `qemu:///system` | NAT network and DHCP leases exist only there. The user is in group `libvirt`, so the socket needs no sudo and no polkit. |
| Reset points | Native libvirt internal snapshots, NVRAM in qcow2 format | Disk and firmware in one atomic operation. No sudo, no `.bak` files. |
| Leases | Linked clone: thin qcow2 overlay on a flat, immutable stock file | Created in milliseconds. The stock file never changes. |
| Lease identity | systemd credentials over SMBIOS type 11 | Nothing per lease is written into the stock image. No libguestfs. fw_cfg is no option: QEMU limits its names to 55 characters. |
| Lease address | AF_VSOCK first, DHCP lease by MAC as fallback | Unique per domain, no DNS race. mDNS is not used for leases. |
| SSH | OpenSSH `ssh`/`scp` as child processes | The agent gets the same command werft used. vsock works through `systemd-ssh-proxy`. |
| Clone | Domain XML generated in Go | `virt-clone` goes away, and with it the mise-python `gi` failure. |
| Setup-specific steps | A recipe file that the setup repo owns (accepted). Until the setup repo takes it over, werft ships it as `examples/setup/recipe.toml` | werft holds no knowledge of the bootstrap repo, the key stick, or the system checks. |

## Packages

`cmd/werft` is thin; all policy is in `internal/`. libvirt and SSH sit behind interfaces, so the
policy packages run against fakes. Same shape as revier: ports outside, one place that wires the
real adapters.

- **`cmd/werft`** — Entry point. One file per command group: flag parsing, text and JSON
  rendering, the map from error kind to exit code, the confirmation prompt. It builds the real
  adapters in one place. No command logic.
- **`internal/failure`** — The error kinds of [Agent interface](#agent-interface). Every package
  gives its errors a kind here; `cmd/werft` maps the kind to the exit code.
- **`internal/virt`** — The hypervisor port: the interface and the types that cross it. Only the
  calls werft needs: domain lookup/list/define/start/shutdown/destroy/undefine, state, XML,
  metadata and description; snapshots, and the redefine of snapshot metadata; pools and volumes,
  and the volume list of a pool; DHCP leases; screenshot, send-key,
  device attach/detach; libvirt version and total host memory. Objects are addressed by name.
  Metadata and description go to the persistent definition.
- **`internal/virt/libvirt`** — The go-libvirt adapter. It opens `libvirt-sock` (monolithic daemon)
  or `virtqemud-sock` (modular daemons) and maps libvirt error codes to failure kinds. libvirt
  reports a taken domain or snapshot name as a generic "operation failed"; the adapter maps it to
  `conflict` by its message, because a define must reserve a lease name.
- **`internal/virttest`** — The fake hypervisor: in-memory domains, volumes and snapshots, a call
  log, and failure injection.
- **`internal/domxml`** — Pure functions that read, build and edit domain XML with libvirtxml.
  They read disks, loader, NVRAM, UEFI, vsock CID, MACs, description and the LUKS marker. They
  build clones of build VMs (new name, UUID, MAC), the qcow2 NVRAM rewrite for migration, and
  lease domains (`domxml.Lease`).
- **`internal/guest`** — The guest port and its OpenSSH adapter: run a command, run a script with
  data on stdin, copy files, probe readiness. A secret goes on stdin, never on the command line. A
  copy empties the target directory and sends the files as one tar stream over one login. The
  adapter passes a werft-owned `ssh_config` with `-F`, so the user's `SendEnv LC_*` never applies.
  It keeps login attempts few, because sshd `PerSourcePenalties` punishes failures: a wait for SSH
  pauses 1 s after the first attempt and doubles the pause up to 8 s.
- **`internal/guest/assets`** — Generic guest helpers, embedded in the binary with `go:embed`.
- **`internal/console`** — Typing at the boot console: screenshot, decode with Go `image/png`, detect
  "not black and still for 4 frames", send key codes with `DomainSendKey`. No keystroke passes
  through a `virsh` process. The timings are those of `vm`: a screenshot every 2 s, at most 120 s.
  The wait stops when its context ends, so SIGINT stops it before anything is typed.
- **`internal/lifecycle`** — `list`, `start` and `stop` for any domain. `list` gets disk use from
  the storage API and counts native reset points plus the `.bak` files of `vm`. It finds the `.bak`
  files in a listing of the NVRAM directory, as `vm` did, and not through the storage pool: `vm`
  writes them outside the pool API, and libvirt refreshes a pool only on request. It needs no root.
  The address is `vsock/<cid>`, else the IPv4 DHCP lease (`virt.DomainAddresses`, the same rule
  that `prepare` and `provision` use). `start` opens no viewer without `--view`, does not type a
  disk password, and waits 60 s for a DHCP lease newer than the start: libvirt keeps a lease until
  it expires, also after the domain stopped. It gives the address only when a lease came; no lease
  is no error. `stop` waits 120 s for the ACPI shutdown, then fails with `timeout`.
- **`internal/resetpoint`**, **`internal/image`**, **`internal/lease`** — Reset points, raw-NVRAM
  guard and migration; stock image catalog and seal; lease lifecycle and reap. See the sections
  below.
- **`internal/pipeline`**, **`internal/steps`** — The step runner for `prepare` and `provision`, and
  the built-in steps ([Provision pipeline](#provision-pipeline)).
- **`internal/ansi`** — A writer that strips terminal escape sequences, for the provision log.
- **`internal/recipe`** — Loads and validates a recipe (TOML, `github.com/BurntSushi/toml` as in
  revier). A recipe error fails before any step runs.
- **`internal/secret`** — Reads a secret from `pass`, else a TTY prompt, else fails with
  `secret_unavailable`. Holds it in memory only and gives it to steps as stdin data.
- **`internal/hostdev`** — Host side of USB passthrough: `findmnt`, `udevadm`, `fuser`,
  `udisksctl`. Finds the stick, makes sure no host process uses it, and unmounts and releases it.
- **`internal/config`**, **`internal/state`**, **`internal/build`** — `config.toml` (pool,
  memory budget, recipe per build VM; the URI is fixed to `qemu:///system`); paths, locks and image records under
  `~/.local/state/werft/`; the ldflags version stamp.
- **`internal/livetest`** — Live-test helpers behind build tag `live`
  ([TESTING.md](TESTING.md#write-a-live-test)).

## Data model

### Objects

- **Build VM.** A libvirt domain a human installed from vendor media, for example `ubuntu-26-04`,
  plus its provision result `ubuntu-26-04-full`. These are the domains `vm` manages today. Only
  build VMs have reset points. Leases never run on them.
- **Reset point.** A native libvirt internal snapshot of a shut-off build VM. It holds disk and
  NVRAM. Fixed labels carry over: `clean-install`, `provision-base` (made by `prepare`), `clean`
  (on `-full`).
- **Stock image.** A flat qcow2 disk file plus a qcow2 NVRAM vars file, made from a build VM reset
  point by `seal`. Named `<image>-<tier>-<YYYYMMDD>[-n]`. Immutable: werft never attaches it to a
  domain or writes it. A new seal makes a new version. werft has no stock garbage collection yet:
  old versions stay ([Stock garbage collection](#stock-garbage-collection) is the design).
- **Tier.** `plain` comes from the `provision-base` reset point of the build VM. `full` comes from
  the `clean` reset point of the `-full` build VM.
- **Lease.** A domain `werft-<image>-<tier>-<id>` with a thin overlay on one stock version, its own
  NVRAM, an owner, a key pair, and an expiry time.

### Where state lives

| State | Location | Source of truth |
|---|---|---|
| Build VMs, leases | libvirt domains | libvirt |
| Lease owner, created, expiry, stock version, state directory | `<metadata>` element of the lease domain, werft namespace | libvirt |
| Reset points | libvirt snapshot metadata plus qcow2 internal snapshots in disk and NVRAM | libvirt |
| Stock files, lease overlays, `clean-install` exports of `migrate` | storage pool `werft`, `/var/lib/libvirt/images/werft/` | libvirt |
| Lease NVRAM | libvirt per-domain NVRAM, filled from the stock vars template at first start | libvirt |
| LUKS marker | domain description line, same format as `vm` writes | libvirt |
| Image records (user, guest family, systemd version, versions, the domain definition leases start from) | `~/.local/state/werft/images.toml`, written by `seal` | werft |
| Lease key pair, lease `ssh_config` | `~/.local/state/werft/leases/<id>/`: `id_ed25519`, `id_ed25519.pub`, `ssh_config` | werft, removed on release |
| werft key for build VMs (installed by `prepare`), build VM `ssh_config` | `~/.local/state/werft/id_ed25519`, `~/.local/state/werft/ssh_config` | werft |
| Provision logs | `~/.local/state/werft/logs/provision-<name>-<YYYYmmdd-HHMMSS>.log` | werft |
| Locks | `~/.local/state/werft/locks/` | werft |
| Config | `~/.config/werft/config.toml` | user |
| Recipes | setup repo; until then `examples/setup/` in werft | setup repo |

werft never opens a file under `/var/lib/libvirt/` directly. Every disk operation goes through the
storage API, because `/var/lib/libvirt/images` is `0711 root`. The one exception: `list`,
`snapshots` and the raw-NVRAM guard read the file names in the NVRAM directory
(`/var/lib/libvirt/qemu/nvram` is `0755`) to find the `.bak` files of `vm`. werft creates the
`werft` pool on first use with `StoragePoolDefineXML` and builds it through the libvirt API, with no
sudo. libvirt keeps the NVRAM files in no pool by itself. When no pool lists an NVRAM file in
`/var/lib/libvirt/qemu/nvram`, `clone`, `migrate` and `seal` refresh the pool on that directory,
or define the pool `nvram` on it the same way when no pool has that target, and look again
(`virt.NVRAMVolume`). libvirt allows one pool per directory. They do this only in their run, after
the questions: a check and the plans of `migrate` and `seal` change nothing (`virt.FindNVRAM`).

## Lease lifecycle

`werft lease <image> [--tier plain|full] [--stock name] [--memory mib] [--cpus n] [--for duration]
[--owner o] [--view]` runs these steps (`lease.Start` in `internal/lease/lease.go`). The flags can
stand before or after the image. The disk password of step 6 is not built yet.

`cmdLease` in `cmd/werft/lease.go` checks the flags before it reads `images.toml` or calls libvirt,
and fails with `usage` outside these bounds:

- `--memory` is in MiB, at least 256 (`lease.MinMemory`). It catches a size in GiB where MiB is
  meant. The host memory is the upper bound, through admission.
- `--cpus` is at least 1. libvirt checks the upper bound when it defines or starts the domain.
- `--for` is from 15 minutes (`lease.MinLifetime`) to 24 hours (`lease.MaxLifetime`). The address
  and SSH waits of steps 7 and 8 take up to 10 minutes, and the reap pass of another werft run must
  not release a lease that still boots. `werft extend` adds time past 24 hours.

1. **Resolve.** Image and tier give the newest stock version in `images.toml`. `--stock` gives the
   version of that name: `not_found` when `images.toml` has none, `usage` when it is of another
   image or tier. A record without a domain definition (sealed before leases existed) fails with
   `unsupported` and names the seal. `--memory` and `--cpus` change the `<memory>`,
   `<currentMemory>` and `<vcpu>` of the domain definition (`domxml.Resize`); a CPU topology
   becomes one socket with that many cores. A definition with NUMA cells or a `<maxMemory>` fails
   with `unsupported`. `--cpus` also fails with `unsupported` when the `<cputune>` of the
   definition names vCPU ids (`vcpupin`, `vcpusched`, `cachetune`, `memorytune`).
2. **Admit.** Take the global lock (`lease.admit`). Fail with `no_capacity` when host memory less
   committed memory less the `<memory>` of the lease definition is below `memory_budget_mib` in
   `config.toml` (default 4096), or when `StoragePoolGetInfo` gives the stock pool less than 1 GiB
   free (`lease.MinPoolFree`).
   - Host memory is the total of `NodeGetInfo`.
   - Committed memory (`lease.committed`) is the configured `<memory>` of every domain that is not
     shut off, plus every lease domain in any state, whatever its state directory.
   - Free memory (`NodeGetFreeMemory`) is no input: it changes with the page cache, and it does not
     drop for a lease that is defined but not started, or for a guest that has not touched its
     memory yet.

   The lock stays held until step 5 ends, so the next admission counts the new lease domain, and
   reap never sees a lease directory whose domain is not yet defined.
3. **Prepare.** Create a lease ID (6 lowercase letters and digits), the lease directory, and an
   ed25519 key pair in it.
4. **Disk.** `StorageVolCreateXML` of `<domain>.qcow2` in the pool of the stock version, with a
   `backingStore` on the stock file. Thin, created in milliseconds.
5. **Define.** `DomainDefineXMLFlags` with the unique name. This reserves the lease atomically. The
   XML is the domain definition of the stock record, with a new UUID and a random MAC, the overlay
   as its only disk, no title and no description. It holds:
   - the explicit loader of the stock definition, without firmware autoselection, and
     `<nvram template=… templateFormat='qcow2' format='qcow2'>` with the stock vars as template,
     so the lease keeps the installer's boot entries. libvirt makes the NVRAM file at the first
     start;
   - `<vsock><cid auto='yes'/>`;
   - a serial or console device with a file source gets a pty, and every `<log>` goes: a file of
     the stock definition is one path for every lease, and the second lease that starts fails to
     open it;
   - SMBIOS credentials: `system.hostname` (the domain name), `system.machine_id` (new),
     `ssh.ephemeral-authorized_keys-all` (the lease public key, for every user over vsock), and
     `werft.ssh.authorized_keys` for the TCP path. A `tmpfiles.extra` credential appends that key
     to `/home/<user>/.ssh/authorized_keys` at each boot, and a
     `systemd.unit-dropin.systemd-tmpfiles-setup.service` credential adds
     `ImportCredential=werft.*`, because `systemd-tmpfiles-setup` imports no other credential.
     Known limits: unit drop-ins from credentials need systemd 256, so the TCP path does not reach
     the key in an older guest (seal needs systemd 256 too, so no such image exists today); the key
     line is appended again at each boot; the home directory is assumed to be `/home/<user>`;
   - werft metadata in namespace `https://github.com/hk9890/werft/lease`: owner, created, expiry
     (created plus `--for`, default 4 hours), stock version, and the state directory of the werft run.

   Then release the global lock. Steps 2 to 5 take seconds and do not include the boot.
6. **Boot.** `DomainCreateWithFlags`. The domain keeps its graphics device, but no viewer opens
   unless `--view`. When the stock image carries the LUKS marker, type the disk password as the
   `boot` step does; it comes from `pass`, so a lease without a TTY needs the GPG agent unlocked.
7. **Address.** vsock CID from the live XML when the image record says the guest has systemd 256
   or later. Else the DHCP lease for the MAC (`NetworkGetDhcpLeases`), within the boot timeout.
   Write the lease `ssh_config`: `Host <id>` with that address, the image's user and the lease key.
8. **Ready.** Probe SSH through the lease `ssh_config` until it answers or the boot timeout (5
   minutes) ends. Few attempts, with growing intervals.
9. **Hand over.** Print the lease ID, domain, stock version, owner, SSH user, address, key path, a
   ready `ssh` command (`ssh -F <ssh_config> <id>`), the path of the lease `ssh_config`, the
   creation time and the expiry.

A lease that fails after step 3, SIGINT included, is released before werft exits; the error keeps
the kind of the failure. When an overlay delete fails and no domain was defined, `werft release`
cannot find the overlay, so the error names the `virsh vol-delete` command that removes it.

The owner is the `--owner` flag, else `$WERFT_OWNER`, else `$USER`. It is a label for `werft
leases` and the reap output. Release does not check it: the lease ID is the handle, and the host
has one user.

`werft release <lease-id>` (`lease.Release`) finds the lease domain by its name
`werft-*-<id>` and its werft metadata; a domain without the metadata is no lease and stays. It
destroys the domain, deletes the overlay, undefines the domain with its NVRAM, and removes the lease
directory. The domain is the only way to find the overlay, so a failed overlay delete keeps the
domain and the lease directory, and fails; the next release tries again. Each step accepts
"already gone", so release can run twice, exits 0 for a lease that is gone, and finishes a
half-done release.

`lease`, `leases`, `extend` and `release` first release the expired leases and the orphans (a
lease domain without a lease directory, or the reverse). `werft reap` does only that (`lease.Reap`
in `internal/lease/reap.go`). `list` has no reap pass: it changes nothing. No timer runs
`werft reap`, so an expired lease stays until one of these five commands runs with its state
directory. Reap rules:

- It takes the global lock, so it never sees a lease directory whose domain is not yet defined.
- A lease domain is a domain named `<prefix><image>-<plain|full>-<id>` with werft lease metadata
  (`lease.List`). A domain without the metadata, or with another name, is no lease and stays.
- A lease belongs to the state directory its metadata names, compared also after symlinks.
  `leases`, `extend` and reap act on the leases of their own state directory, and on the leases
  without one (made before the element existed). A live test runs with an isolated state
  directory, so a werft run with the real one never acts on a live-test lease, and the reverse.
- A lease expires at its expiry time. A lease without a readable expiry never expires. Reap
  releases an expired lease of its own state directory or of none.
- A lease domain is an orphan only when its metadata names this state directory and its lease
  directory is missing. A lease without a state directory is never an orphan: its lease directory
  can be in any state directory.
- A lease directory is an orphan when it is under `leases/`, is named like a lease ID, and no lease
  domain has that ID, whatever state directory the domain names.
- It releases a lease as `werft release` does, so a failed overlay delete keeps the domain and the
  lease directory, and the next reap tries again. It goes on with the next lease and names every
  failure. `werft reap` then fails with `failed`; the reap pass of the other commands only warns
  on stderr. `werft release` takes no global lock, so a domain that goes during the reap counts
  as released.

`werft leases` lists each lease: ID, domain, image and tier (from the domain name), stock version,
owner, created, expiry (from the metadata), and the user, address, key, `ssh_config` and `ssh`
command of the lease directory. A lease that still boots has no address yet.

`werft extend <lease-id> [--for duration]` (`lease.Extend`) adds `--for` (default 4 hours) to the
expiry, under the global lock, and writes the metadata again with `SetDomainMetadata`. An expired
lease is `not_found`.

## Reset points

### Native snapshots with qcow2 NVRAM

`snapshot` and `reset` use `DomainSnapshotCreateXML` and `DomainRevertToSnapshot` on a shut-off
build VM. With qcow2 NVRAM, libvirt puts disk and NVRAM into one snapshot. This needs libvirt 11.4
or later; the host has 12.0. Results:

- Invariant "both halves or neither" moves into libvirt. The rollback after a half-done snapshot
  and the unrecoverable half-done reset go away.
- `snapshot`, `snapshots`, `reset` and `migrate` need no sudo.
- After migration, `list` counts only native reset points. Until then it also counts the `.bak`
  files of `vm`.
- The VM must still be shut off. werft takes no memory snapshots.
- `--replace` still deletes before it creates. libvirt cannot rename a snapshot.

The commands keep the interface of `vm`: `snapshot [--replace] <name> [label]`,
`snapshots <name>` and `reset [--yes] <name> [label]`, with the default label `clean-install`.
`snapshot` and `reset` take the domain lock. `reset` makes all checks (shut off, guard, label
exists) before it asks; without a TTY it needs `--yes`. `snapshots` also lists the `.bak` labels
of `vm`, and names `werft migrate` for them.

### Raw-NVRAM guard

With raw NVRAM, libvirt snapshots a shut-off UEFI domain without error and leaves the NVRAM out.
Before every snapshot create or revert, werft reads the domain XML and refuses when:

- the domain has a UEFI loader and its NVRAM format is not `qcow2`;
- a disk that werft snapshots is not qcow2;
- the libvirt version is below 11.4.

The error names `werft migrate <domain>`. A BIOS domain (no NVRAM) passes.

It also refuses a domain with a qcow2 NVRAM that has a `.bak` file of `vm` next to it. Then `vm
snapshot` ran after the migration, and the disk holds a `qemu-img` snapshot that libvirt does not
know. The storage API does not show the internal snapshots of a disk, so the `.bak` file is the
only sign werft can see. The error names the commands that remove the snapshot.

### One-time migration

`werft migrate [--yes] <domain>` moves a build VM from `qemu-img` snapshots plus `.bak` files to
native reset points. Both formats use the qcow2 internal snapshot table of one disk, and the
labels are equal, so migration keeps one label per domain:

- **Kept:** `provision-base`, or `clean` on a `-full` domain. It becomes a native reset point.
  Without it, `clean-install` is kept.
- **Exported:** `clean-install`, the untouched vendor install, when it is not kept. It goes to two
  flat qcow2 volumes in the `werft` pool (`pool` in `config.toml`): `<domain>-clean-install.qcow2`
  (disk) and `<domain>-clean-install_VARS.qcow2` (NVRAM).
- **Deleted:** every other label.

`vm` took its `qemu-img` snapshots of the first disk only, and marks each with
`<nvram>.<label>.bak`. migrate finds the labels in the volume list of the NVRAM pool, after a pool
refresh. When no pool lists the NVRAM yet, it finds them in a listing of the NVRAM directory, and
makes the pool only after the confirmation. It needs no root:

- libvirt accepts the metadata of a `qemu-img` snapshot that exists in the disk
  (`DomainSnapshotCreateXML` with the redefine flag). It then reverts and deletes the snapshot
  through `qemu-img`, as root. A redefine does not check the disk; a revert to a snapshot the disk
  lacks fails. A delete of a snapshot the disk lacks passes.
- The `.bak` files and the raw NVRAM are volumes of the NVRAM pool. `StorageVolCreateXMLFrom`
  converts them to qcow2, and `StorageVolDelete` removes them.

Steps:

1. Check before any change: shut off, libvirt 11.4 or later, UEFI with an NVRAM, every writable
   disk qcow2, disk and NVRAM in a storage pool, an explicit loader path, no export volume that an
   earlier run of this migration did not make. Take the domain lock, then confirm. Revert the disk
   to the kept label: this proves that the disk has it, before anything is deleted.
2. Export `clean-install`: revert the disk to it, copy the disk and the `.bak` to the `werft` pool,
   then delete the `.bak` and then the label. A copy is flat: it has no internal snapshots.
3. Delete each other label: first its `.bak`, then the label.
4. Revert the disk to the kept label. Make `<domain>_VARS.qcow2` in the NVRAM pool from its `.bak`.
   Redefine the domain with `format='qcow2'` and the explicit loader; libvirt fills the firmware
   features in again from the descriptor of that loader. A failed redefine deletes the new NVRAM.
5. Delete the raw NVRAM and the `.bak` of the kept label, then the kept `qemu-img` snapshot. Take a
   native reset point with the kept label.

Each step can run again. A run that stops before step 4 starts again from the `.bak` files and from
the snapshot metadata it redefined; the metadata of a label goes after its `.bak`. An export volume
that exists is complete, because libvirt deletes a copy that fails. It is from an earlier run only
while libvirt has the redefined `clean-install`; else migrate refuses with `conflict` and names the
volume. A run that
stops in step 5 continues there: the kept label is still the redefined snapshot of `vm`, and its
domain names the raw NVRAM. When the native snapshot fails, the error names
`werft snapshot <domain> <label>`.

A domain with neither the kept label nor `clean-install` keeps its current disk and NVRAM: step 3
runs, the qcow2 NVRAM is a copy of the current one, and the domain gets no reset point.
`werft prepare` or `werft snapshot` takes one.

After migration `vm reset` and `vm provision` refuse the domain, because they find no `.bak` file.
`vm snapshot` does not refuse it: it finds the native reset point with `qemu-img snapshot -l`, and
takes a `qemu-img` snapshot plus a `.bak` copy of the qcow2 NVRAM. `vm snapshot --replace` of a
native label deletes the disk half of the libvirt reset point. Do not use `vm snapshot` on a
migrated domain. The raw-NVRAM guard catches the `.bak` file of such a snapshot. The two tools run
side by side, one per domain.

## Stock images and seal

`werft seal [--yes] <build-vm> <label>` makes a new stock version (`image.Plan`, `Seal.Run` in
`internal/image/seal.go`). The build VM and the label give the tier (`tierOf`), and seal refuses
every other pair with `usage`:

- Label `provision-base` on a build VM that is not `-full` gives the `plain` tier of the image with
  the build VM name.
- Label `clean` on a `-full` build VM gives the `full` tier of the image with the build VM name
  without `-full`: `werft seal ubuntu-26-04-full clean` makes `ubuntu-26-04-full-<YYYYMMDD>`, image
  `ubuntu-26-04`.

Both tiers run the same steps:

1. Check before any change: the label exists, and the build VM is shut off and passes the
   raw-NVRAM guard. The revert brings back the definition the reset point holds, so check that one
   (`SnapshotXML`): one writable disk in a storage pool, a UEFI NVRAM, and no LUKS marker. Then
   confirm: the revert loses what the build VM wrote since the label.
2. Revert the build VM to the label, and read its definition again. A disk or NVRAM that differs
   from the checked definition fails the run before anything is copied.
3. Under the global lock, pick the stock name: the first `<image>-<tier>-<YYYYMMDD>[-n]` that no
   record, no volume of the pool and no domain has. Define a temporary domain under that name, so
   the name is reserved: the build VM XML on the stock volumes `<name>.qcow2` and
   `<name>_VARS.qcow2` in the `werft` pool (`pool` in `config.toml`), with a new UUID and MAC, a
   vsock device, and SMBIOS credentials that replace those of the build VM. The credentials are a
   new key pair of the run as `ssh.ephemeral-authorized_keys-all`, and a `sshd-vsock@` drop-in that
   runs `ssh-keygen -A`, because a cloud image has no host keys.
4. Without the global lock, copy disk and NVRAM to the stock volumes with
   `StorageVolCreateXMLFrom`; the copy is flat, with no internal snapshots.
5. Boot the temporary domain and log in as root over vsock with the key of the run. This needs no
   guest password, and needs systemd 256 or later.
6. Run the embedded seal script (`internal/guest/assets/seal.sh`): print the guest family and the
   systemd version; empty `/etc/machine-id`; delete `/etc/hostname`, `/etc/ssh/ssh_host_*` and the
   werft key `prepare` installed for `$USER`; add the host-key regeneration (required: a guest
   without host keys accepts the vsock connection and closes it at once) as a `sshd-vsock@`
   drop-in and as a oneshot that `ssh` and `sshd` want; give `$USER` passwordless sudo; schedule
   the poweroff 2 s later with `systemd-run`. It fails when the guest has no user `$USER`. The
   tmpfiles line for `werft.ssh.authorized_keys` comes with the lease, which passes that
   credential ([Lease lifecycle](#lease-lifecycle), step 5).
7. Wait for the poweroff. No SSH login comes after the script: it would start a `sshd-vsock@`
   instance whose drop-in makes new host keys, and every lease of the image would share them.
   Undefine the temporary domain, keep the volumes under the stock name, and append the record to
   `images.toml` under the global lock. The record keeps the definition of the temporary domain
   without its credentials; leases start from it, so they do not depend on the build VM.

A failed run removes the temporary domain and both volumes. When that removal fails, the error
names what is left.

`werft images --json` gives every field of each record but the domain definition: name, image,
tier, version, date, source build VM and label, pool, volumes, user, guest family and systemd
version. The text table shows name, image, tier, version, date and user.

`TestSealLive` in `cmd/werft/image_live_test.go` proves the result. It reads the stock disk with
`debugfs` and finds no host keys, no hostname and an empty machine-id. It boots two overlays of the
stock disk and finds a different machine-id and different host keys in each. `TestSealFullLive`
seals the `clean` reset point of a scratch `-full` domain, and its `--tier full` lease logs in with
a new machine-id and holds the files of `clean`.

Build VMs and `-full` stay without passwordless sudo, as today. Only stock images get the seal
changes. Seal removes no secrets: a `full` lease holds them by design
([PRODUCT.md](PRODUCT.md#access-and-trust)).

### Stock garbage collection

werft has no stock garbage collection yet; this is its design. Under the global lock, it deletes
the disk, the NVRAM and the record of each stock version that matches all of these:

- It is not the newest version of its image and tier.
- No lease domain names it in its metadata.
- No volume of the pool has it as backing file. An overlay can stay without its lease domain when
  a lease fails half-way.
- No seal uses it: a version whose name a temporary seal domain reserves has no record yet.

Admission finds the stock record again under the global lock, before it creates the overlay, so a
version that the lease chose before the lock cannot go in between.

## Provision pipeline

### Step model

`prepare` and `provision` are lists of steps run by `internal/pipeline`. A step has:

- a **check**, run in preflight before any step runs: validate inputs, queue questions, declare
  secrets. The runner asks the queued questions after every check passed, so a failed check, such
  as a key stick that a program holds, costs no question. All questions come before any secret and
  any destructive action.
- a **run**.
- an optional **cleanup**, pushed on a stack before the run starts, so a half-done step is undone.
  On success, failure and SIGINT the runner pops the stack in reverse. Each cleanup must be safe
  to run twice. `undo` takes a cleanup off the stack only when it succeeds, so a failed one is
  tried again at the end. The `viewer` cleanup runs only through `undo` and after a successful
  run; a failed run leaves the window open on the VM it leaves running. With no display, or when
  the viewer fails, a step names `virt-viewer -c qemu:///system --wait --reconnect <name>` to run
  on the desktop. That command changes nothing; `werft start --view` would start the VM.

The runner catches SIGINT and SIGTERM only after preflight and the secrets. An interrupt during a
question, a secret prompt or `pass` ends werft before any change. After that, the runner checks
for an interrupt before each step, and an interrupt ends every wait of a step at once.

Steps share one run context (domain, address, guest family, secrets, log writer). On failure the
runner stops, leaves the VM running, and prints the SSH command and log path. The log file gets
plain text without ANSI codes.

### Built-in steps

All generic.

| Step | Does |
|---|---|
| `reset` | Force off the VM and revert it to a reset point |
| `boot` | Start the VM; type the disk password when the LUKS marker is set. A VM that runs already gets no keys: its screen can be a desktop |
| `viewer` | Open a viewer; `undo` or a successful run closes it, a failed run leaves it open |
| `wait-ssh` | Wait until sshd answers; `prepare` also installs the werft key and the user's own key |
| `verify-guest-password` | Check the guest password with one `sudo` call; a lost SSH connection is `failed` and blames no password |
| `detect-guest` | Detect the family (`ubuntu` or `omarchy`) and the session process |
| `hostname` | Set the guest hostname to the domain name |
| `luks-mark` | Probe the root disk for LUKS and set or remove the LUKS marker line; the rest of the description, blank lines too, stays. A probe whose connection drops fails the step and keeps the marker |
| `autologin` | Turn GDM autologin on, and off again in cleanup; `prepare` checks it is off |
| `upload` | Copy the embedded helpers and the recipe files into the guest |
| `wait-session` | Wait for the desktop session |
| `unlock-keyring` | Unlock the GNOME keyring |
| `sudo-grant` | Grant passwordless sudo, and revoke it in cleanup |
| `usb-passthrough` | Find the stick by its mount label under `/media/$USER` or `/run/media/$USER`, or at the mount point `provision --key <mount>` gives; refuse it in preflight when a program holds it, unmount it on the host, attach it live only, and give it back to the host in cleanup. A failed detach leaves the stick in the guest, mounts nothing on the host, and is tried again at the end of the run. A VM that is off, or booted again, has let go of the stick, and the host mounts it |
| `pinentry` | Install a pinentry stub with the passphrase in tmpfs and a gpg-agent line; remove them in cleanup |
| `run` | Run a guest command or script, with an optional secret on stdin |
| `checks` | Copy a host check directory into the guest and run its entry script; a script that fails is `checks_failed`, a lost SSH connection is `failed`. ssh exits 255 for both, so a script that exits 255 is `failed` |
| `reboot`, `poweroff` | Power off from inside the guest; `reboot` then boots and waits for SSH |
| `clone` | Refuse a source that is not shut off (a `poweroff` step comes first), delete the old clone, define a new one from generated XML, copy the viewer settings; the steps after it act on the clone |
| `generalize` | Boot the clone, give it a new machine-id, new SSH host keys and its hostname, power it off |
| `snapshot` | Take a reset point |
| `undo` | Run the cleanup of an earlier step now, so a temporary change ends where the recipe says |

`prepare` is a fixed built-in step list; it needs no recipe. Its two waits for a person (sshd install,
disk password) stay, with a hint for each. The disk password wait ends with a DHCP lease newer than the
start, as `lifecycle.WaitAddress` does: a vsock CID is in the live XML from the start, and libvirt
keeps the lease of an earlier boot, so neither is a sign that the guest left its initramfs.

The clone gets a copy of each writable disk and of the NVRAM, made with `StorageVolCreateXMLFrom`
in the pool of the original (the NVRAM in the `nvram` pool). An NVRAM template does not work:
firmware autoselection refuses a qcow2 template. A read-only or shareable disk, such as an
installer ISO, stays shared. Preflight finds the volume of each writable disk and of the NVRAM; one
outside every storage pool fails there, before anything changes. An NVRAM in the NVRAM directory
that no pool lists yet passes preflight; the run makes a pool list it before it removes the old
clone. `clone` refuses a running source build VM with `conflict`: libvirt copies the disk of a
running guest without an error, and the copy is inconsistent. A copy that fails deletes the copies
made so far. Before `clone` removes the old
`<name>-full`, it checks again that it is shut off. It deletes the old NVRAM through the storage
API: an undefine that deletes the NVRAM unlinks the file, but its pool keeps the name, and the new
copy under that name fails. It also deletes the `.bak` NVRAM copies of the old clone's `vm` reset
points, as `vm` did.

The guest login is `$USER` with the werft key and `~/.ssh/id_ed25519`, through the werft
`ssh_config`. It sets `IdentitiesOnly`, so no other agent key uses up sshd's `MaxAuthTries`.
`prepare` tries one login with both keys. When the guest refuses both, `prepare` runs `ssh-copy-id`
with the werft key, without the user's `ssh_config` and with no key offered, so it asks for the
password; so at most one failed login comes before it, as
in `vm`. Then `prepare` adds each public key that is not in `~/.ssh/authorized_keys` yet: the werft
key and `~/.ssh/id_ed25519.pub`, when it exists. The user's key keeps `ssh <build VM>` working, and
the clone inherits both. Last, `prepare` checks the werft key alone (`ssh -P werft-key-only`, which
the config maps to that key). The address is the vsock CID when the domain has one, else the IPv4 DHCP lease of each MAC;
the first that answers SSH wins. A cleanup reaches its domain again by the same rule, because the
guest can reboot and the run can move on to the clone after the step ran.

### Recipe

A recipe is a TOML file that lists the provision steps and their parameters. The setup repo owns
it and the files it names; paths resolve relative to the recipe, and `~/` is the home directory. A
step can be limited to a guest family. `config.toml` maps each build VM to its recipe (`recipe` for
all, `[recipes]` per build VM); `--recipe` overrides it. `prepare` reads only `[guest_password]`
from it; without a recipe it says so and asks for the guest password at the terminal.

```toml
[guest_password]              # pass entry, else the prompt at the terminal
pass = "vm/guest-password"

[secrets.passphrase]          # a named secret for run and pinentry
prompt = "GPG passphrase"

[[steps]]
type = "sudo-grant"           # a built-in step
id = "sudo"                   # optional; the name undo uses, default the type
family = "omarchy"            # optional; needs a detect-guest step before it
```

Parameters: `reset` takes `label` (default `provision-base`); `snapshot` and `usb-passthrough`
take `label`; `upload` takes `files`; `run` takes `command` (run in the guest directory) and
`secret` (on stdin); `checks` takes `dir` and `command` (default `run.sh`); `pinentry` takes
`secret`; `undo` takes `step`. Load fails on an unknown key, a missing file, an unknown secret, a
`family` before `detect-guest`, `generalize` before `clone`, a second `clone`, and an `undo` without an earlier step
that has a cleanup. The setup recipe holds:

- the bootstrap repo URL and the script that clones it and runs `install.sh` (the rest of
  `guest/bootstrap.sh`);
- `tests/system` for the `checks` step, from the host checkout as today;
- `usb-passthrough` with mount label `MASTER-KEY`, and `pinentry` with a TTY secret;
- the guest password secret from `pass` entry `vm/guest-password`;
- for family `omarchy`: a `run` step for `omarchy update` (`OMARCHY_UPDATE_LOGGED=1`,
  `env-bootstrap`), then `reboot`;
- `autologin` and `unlock-keyring`, because the setup writes gsettings and uses `gh`.

Secrets come from `pass` or a TTY prompt. The positional stdin contract of `vm` (passphrase on
line 1, guest password on line 2) goes away.

## Agent interface

- `--json` on every command: one JSON document on stdout. Progress, prompts and logs go to stderr.
- On error with `--json`, stdout holds `{"error": {"kind": "...", "message": "..."}}`, with the
  kind from the table below.
- Without a TTY no command prompts; a missing confirmation fails and names the flag (`--yes`).
  `-y` is short for `--yes` on `reset`, `prepare`, `provision`, `migrate` and `seal`, as in `vm`.
- JSON holds no secret. It gives the path of the lease private key.
- `werft guide` prints `cmd/werft/guide.txt`, embedded in the binary with `go:embed`; with
  `--json` it prints `{"guide": "<text>"}`. `cmd/werft/guide_test.go` checks against `commands`,
  `lease.Lease` and `exitCodes`: every command the guide names after `werft`, alone or in a list,
  the flags of its example calls, its lease JSON fields and its exit codes.
- `werft completion zsh` prints a zsh completion script generated from `commands` in
  `cmd/werft/cli.go`. `zshArguments` in `cmd/werft/completion.go` maps each placeholder of `args`
  to a completer; a new placeholder needs an entry in `completers`. The script reads names from
  `werft --json list`, `snapshots`, `images` and `leases`; the `leases` call releases expired
  leases first. VM names leave out the entries of `werft --json list` with `"lease": true`, the
  domains with werft lease metadata.

Exit codes are part of the contract:

| Code | Kind | Example |
|---|---|---|
| 0 | `ok` | |
| 1 | `failed` | unexpected libvirt or SSH error |
| 2 | `usage` | unknown flag, missing argument |
| 3 | `not_found` | no such domain, image, lease or reset point |
| 4 | `conflict` | VM not shut off, label exists without `--replace` |
| 5 | `needs_confirmation` | no TTY and no `--yes` |
| 6 | `no_capacity` | pool space or memory |
| 7 | `timeout` | no SSH, no lease address, no shutdown in time |
| 8 | `checks_failed` | the provision checks failed |
| 9 | `unsupported` | raw-NVRAM guard, libvirt version |
| 10 | `secret_unavailable` | `pass` failed and no TTY |

## Concurrency and locking

- libvirt is the arbiter. A unique domain name reserves a lease. QEMU image locks prevent two
  writers on one disk. Stock files are opened read-only as backing files.
- `locks/global.lock` (`flock`) serializes admission, seal, reap, extend, stock garbage collection,
  and the creation of the werft key by `prepare`, between runs with the same state directory: each
  state directory has its own lock. `release` holds it only for its reap pass, not while it releases the lease.
  Admission holds it from the capacity check until the lease domain is defined, so the next
  admission counts the memory of that domain before it starts. Seal holds it while
  it picks the stock name and defines the temporary domain that reserves it, and while it writes
  the record. It is held for seconds: never for a disk copy, never for a boot.
- `locks/<domain>.lock` serializes `snapshot`, `reset`, `prepare`, `provision`, `seal` and
  `migrate` on one build VM. A second run fails with `conflict` and does not wait.
- Leases run in parallel. Limits: the memory budget, pool free space, and the 253 addresses of the
  NAT range.

## Migration from `vm`

The phased plan is in [PRODUCT.md](PRODUCT.md#roadmap). Technical constraints:

- The user migrates one build VM at a time with `werft migrate`. `vm` keeps working on the domains
  not yet migrated and refuses a migrated domain.
- Phase 2 adds a guard to `vm snapshot` in the setup repo, so it refuses a domain with a qcow2
  NVRAM ([One-time migration](#one-time-migration)).
- The setup repo keeps: the recipe, the bootstrap script, `tests/system`, the `pass` entry
  `vm/guest-password`, `scripts/common/wait-for-package-manager.sh` for its own use, and the
  `~/.ssh/config` entries for build VMs. Leases need no ssh config entry; werft writes one per lease.

## Risks

- **go-libvirt has no release tags.** Pin the pseudo-version. Known defect: `DomainScreenshot`
  decodes the MIME type from the empty end-of-stream packet and fails after the image streamed. The
  adapter takes the format from the image header instead. A write error in a stream blocks the
  connection, so the adapter keeps the error from go-libvirt and returns it after the stream ends.
  The `internal/virt` interface keeps a
  `virsh` adapter possible. Live tests catch protocol drift.
- **Firmware autoselection.** All firmware descriptors in `/usr/share/qemu/firmware/` are raw, so
  autoselection can reject `format='qcow2'`. Use an explicit loader path. The live test
  `TestUEFIResetPointRestoresNVRAMAndDisk` proves revert of disk and NVRAM on libvirt 12.0 with an
  explicit loader. `TestSecureBootMigrateLive` proves `migrate`, `snapshot` and `reset` on the shape
  of a virt-manager build VM: Secure Boot with enrolled keys, `OVMF_CODE_4M.ms.fd`, SMM.
- **systemd versions.** vsock SSH and `ssh.ephemeral-authorized_keys-all` need systemd 256.
  Ubuntu 24.04 has 255 and uses the TCP fallback. Seal has no TCP fallback yet, so it cannot seal
  Ubuntu 24.04. A static `/etc/hostname` overrides the hostname
  credential, so seal must delete it.
- **Duplicate DHCP DUID.** Clones with one machine-id get one IP. The machine-id credential
  prevents it only if seal empties `/etc/machine-id`.
- **Desktop internals.** Keyring unlock uses gnome-keyring's
  `InternalUnsupportedGuiltRiddenInterface`, which can change between releases.

## Open questions

- **SSH library.** OpenSSH child processes (proposed), or `golang.org/x/crypto/ssh` with an AF_VSOCK
  dialer: no process per call, but a different path from the one the agent uses.
- **Recipe location.** Where in the setup repo the recipe lives. TOML with a `family` key per step
  is enough for the setup recipe.
- **Scripted secrets.** Is `pass` plus TTY enough, or does an unattended `provision` need a secret
  file or file descriptor? Can an agent run `provision` without a TTY at all, given the key stick
  and the `pinentry` secret?
