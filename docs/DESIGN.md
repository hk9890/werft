# werft — design

How werft is built. What werft must do and for whom: [PRODUCT.md](PRODUCT.md). werft replaces the
bash tool `vm` in [`scripts/vm/`](https://github.com/carlo9890/setup/tree/main/scripts/vm) of the
setup repo.

Status: target design. In code: the hypervisor port with its libvirt adapter and fake, `domxml`,
`failure`, `config`, `state`, the command dispatch with `version` and `help`, and the provision
pipeline with `prepare` and `provision` (`pipeline`, `steps`, `recipe`, `guest`, `console`,
`secret`, `hostdev`, `ansi`, and the snapshot part of `resetpoint`).

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

- **`cmd/werft`** — Entry point. One file per command: flag parsing, text and JSON rendering, the
  map from error kind to exit code, the confirmation prompt. It builds the real adapters in one
  place. No command logic.
- **`internal/failure`** — The error kinds of [Agent interface](#agent-interface). Every package
  gives its errors a kind here; `cmd/werft` maps the kind to the exit code.
- **`internal/virt`** — The hypervisor port: the interface and the types that cross it. Only the
  calls werft needs: domain lookup/list/define/start/shutdown/destroy/undefine, state, XML,
  metadata and description; snapshots; pools and volumes; DHCP leases; screenshot, send-key,
  device attach/detach; libvirt version and free host memory. Objects are addressed by name.
  Metadata and description go to the persistent definition.
- **`internal/virt/libvirt`** — The go-libvirt adapter. It opens `libvirt-sock` (monolithic daemon)
  or `virtqemud-sock` (modular daemons) and maps libvirt error codes to failure kinds. libvirt
  reports a taken domain or snapshot name as a generic "operation failed"; the adapter maps it to
  `conflict` by its message, because a define must reserve a lease name.
- **`internal/virttest`** — The fake hypervisor: in-memory domains, volumes and snapshots, a call
  log, and failure injection.
- **`internal/domxml`** — Pure functions that read, build and edit domain XML with libvirtxml.
  They read disks, loader, NVRAM, UEFI, vsock CID, MACs and description. They will build lease
  domains, clones of build VMs (new name, UUID, MAC), and the qcow2 NVRAM rewrite for migration.
- **`internal/guest`** — The guest port and its OpenSSH adapter: run a command, run a script with
  data on stdin, copy files, probe readiness. It passes a werft-owned `ssh_config` with `-F`, so the
  user's `SendEnv LC_*` never applies. It keeps login attempts few, because sshd
  `PerSourcePenalties` punishes failures.
- **`internal/guest/assets`** — Generic guest helpers in the binary with `go:embed`:
  `wait-for-package-manager.sh`, `unlock-keyring.js`, `pinentry-auto`, the hostname script, the
  generalize script, and the seal script.
- **`internal/console`** — Typing at the boot console: screenshot, decode with Go `image/png`, detect
  "not black and still for 4 frames", send key codes with `DomainSendKey`. No keystroke passes
  through a `virsh` process.
- **`internal/resetpoint`**, **`internal/image`**, **`internal/lease`** — Reset points, raw-NVRAM
  guard and migration; stock image catalog and seal; lease lifecycle and reap. See the sections
  below.
- **`internal/pipeline`**, **`internal/steps`** — The step runner for `prepare` and `provision`, and
  the built-in steps ([Provision pipeline](#provision-pipeline)).
- **`internal/ansi`** — A writer that strips terminal escape sequences, for the provision log.
- **`internal/recipe`** — Loads and validates a recipe (TOML, `github.com/BurntSushi/toml` as in
  revier). A recipe error fails before any step runs.
- **`internal/secret`** — Reads a secret from `pass`, else a TTY prompt, else fails. Holds it in
  memory only and gives it to steps as stdin data.
- **`internal/hostdev`** — Host side of USB passthrough: `findmnt`, `udevadm`, `fuser`,
  `udisksctl`. Finds the stick, makes sure no host process uses it, and unmounts and releases it.
- **`internal/config`**, **`internal/state`**, **`internal/build`** — `config.toml` (pool, lease
  limits, lifetime, recipe per build VM; the URI is fixed to `qemu:///system`); paths, locks and image records under
  `~/.local/state/werft/`; the ldflags version stamp.
- **`internal/livetest`** — Live-test helpers behind build tag `live`: the `werft-test` pool, the
  base image, a scratch UEFI domain, and vsock SSH ([TESTING.md](TESTING.md)).

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
  domain or writes it. A new seal makes a new version; old versions stay until no lease uses them.
- **Tier.** `plain` comes from the `provision-base` reset point of the build VM. `full` comes from
  the `clean` reset point of the `-full` build VM.
- **Lease.** A domain `werft-<image>-<tier>-<id>` with a thin overlay on one stock version, its own
  NVRAM, an owner, a key pair, and an expiry time.

### Where state lives

| State | Location | Source of truth |
|---|---|---|
| Build VMs, leases | libvirt domains | libvirt |
| Lease owner, expiry, stock version | `<metadata>` element of the lease domain, werft namespace | libvirt |
| Reset points | libvirt snapshot metadata plus qcow2 internal snapshots in disk and NVRAM | libvirt |
| Stock files, lease overlays | storage pool `werft`, `/var/lib/libvirt/images/werft/` | libvirt |
| Lease NVRAM | libvirt per-domain NVRAM, filled from the stock vars template at first start | libvirt |
| LUKS marker | domain description line, same format as `vm` writes | libvirt |
| Image records (user, guest family, systemd version, vsock support, versions) | `~/.local/state/werft/images.toml`, written by `seal` | werft |
| Lease key pair, lease `ssh_config` | `~/.local/state/werft/leases/<id>/` | werft, removed on release |
| werft key for build VMs (installed by `prepare`), build VM `ssh_config` | `~/.local/state/werft/id_ed25519`, `~/.local/state/werft/ssh_config` | werft |
| Provision logs | `~/.local/state/werft/logs/provision-<name>-<YYYYmmdd-HHMMSS>.log` | werft |
| Locks | `~/.local/state/werft/locks/` | werft |
| Config | `~/.config/werft/config.toml` | user |
| Recipes | setup repo; until then `examples/setup/` in werft | setup repo |

werft never opens a file under `/var/lib/libvirt/` directly. Every disk operation goes through the
storage API, because `/var/lib/libvirt/images` is `0711 root`. werft creates the `werft` pool on
first use with `StoragePoolDefineXML` and builds it through the libvirt API, with no sudo.

## Lease lifecycle

1. **Resolve.** Image and tier give the newest stock version in `images.toml`.
2. **Admit.** Take the global lock. Read `StoragePoolGetInfo` and free host memory. Fail with "no
   capacity" when the lease memory would take free memory below the budget in `config.toml`, or the
   pool is full. The lock stays held until step 5 ends, so a
   second admission cannot pass the capacity check first, and reap never sees a lease directory
   whose domain is not yet defined.
3. **Prepare.** Create a lease ID, an ed25519 key pair in the lease directory, and a random MAC.
4. **Disk.** `StorageVolCreateXML` with a `backingStore` on the stock file. Thin, created in
   milliseconds.
5. **Define.** `DomainDefineXMLFlags` with the unique name. This reserves the lease atomically. The
   XML holds:
   - `<nvram template=… templateFormat='qcow2' format='qcow2'>` with the stock vars as template,
     so the lease keeps the installer's boot entries;
   - `<vsock><cid auto='yes'/>`;
   - SMBIOS credentials: `system.hostname` (the domain name), `system.machine_id` (new),
     `ssh.ephemeral-authorized_keys-all` (the lease public key), and `werft.ssh.authorized_keys`
     for the TCP path;
   - werft metadata: owner, created, expiry, stock version.

   Then release the global lock. Steps 2 to 5 take seconds and do not include the boot.
6. **Boot.** `DomainCreateWithFlags`. The domain keeps its graphics device, but no viewer opens
   unless `--view`. When the stock image carries the LUKS marker, type the disk password as the
   `boot` step does; it comes from `pass`, so a lease without a TTY needs the GPG agent unlocked.
7. **Address.** vsock CID from the live XML when the image record says the guest has systemd 256
   or later. Else the DHCP lease for the MAC (`NetworkGetDhcpLeases`).
8. **Ready.** Probe SSH with the lease key until it answers or the boot timeout ends. Few attempts,
   with growing intervals.
9. **Hand over.** Print the lease ID, SSH user, address, key path, a ready `ssh` command, the path
   of the lease `ssh_config`, and the expiry.

Release destroys the domain, undefines it with its NVRAM, deletes the overlay, and removes the
lease directory. Each step accepts "already gone", so release can run twice and finishes a
half-done release.

Every command that reads leases first releases the expired ones and the orphans (a lease domain
without a lease directory, or the reverse). `werft reap` does only that.

## Reset points

### Native snapshots with qcow2 NVRAM

`snapshot` and `reset` use `DomainSnapshotCreateXML` and `DomainRevertToSnapshot` on a shut-off
build VM. With qcow2 NVRAM, libvirt puts disk and NVRAM into one snapshot. This needs libvirt 11.4
or later; the host has 12.0. Results:

- Invariant "both halves or neither" moves into libvirt. The rollback after a half-done snapshot
  and the unrecoverable half-done reset go away.
- `snapshot`, `snapshots` and `reset` need no sudo.
- `list` counts reset points from libvirt, not from `.bak` file names.
- The VM must still be shut off. werft takes no memory snapshots.
- `--replace` still deletes before it creates. libvirt cannot rename a snapshot.

### Raw-NVRAM guard

With raw NVRAM, libvirt snapshots a shut-off UEFI domain without error and leaves the NVRAM out.
Before every snapshot create or revert, werft reads the domain XML and refuses when:

- the domain has a UEFI loader and its NVRAM format is not `qcow2`;
- a disk that werft snapshots is not qcow2;
- the libvirt version is below 11.4.

The error names `werft migrate <domain>`. A BIOS domain (no NVRAM) passes.

### One-time migration

`werft migrate <domain>` moves a build VM from `qemu-img` snapshots plus `.bak` files to native
reset points. The two formats cannot share one qcow2 file safely: both use the qcow2 internal
snapshot table, and the labels are equal. So migration keeps one legacy label per domain:

1. Require shut off and confirm. The kept label defaults to `provision-base` (`clean` on `-full`).
2. Optional: export the other legacy labels to flat files in the `werft` pool.
3. Restore the kept label the legacy way (`qemu-img snapshot -a`, `.bak` copy). Last use of sudo.
4. Delete all legacy internal disk snapshots.
5. Convert the NVRAM to qcow2 in the `nvram` pool (`StorageVolCreateXMLFrom`). Redefine the domain
   with `format='qcow2'` and an explicit loader path. Delete the raw NVRAM and the `.bak` files.
6. Take a native reset point with the kept label.

After migration `vm` refuses the domain, because it finds no `.bak` file. This is a safe failure.
The two tools run side by side, one per domain.

## Stock images and seal

`werft seal <build-vm> <label>` makes a new stock version:

1. Revert the build VM to the label. Copy disk and NVRAM with `StorageVolCreateXMLFrom`; the copy
   is flat, with no internal snapshots.
2. Boot a temporary domain on the copy. As root, run the embedded seal script: empty
   `/etc/machine-id`; delete `/etc/hostname`, `/etc/ssh/ssh_host_*` and the key `prepare`
   installed; add the host-key regeneration drop-in (required: a guest without host keys accepts the
   vsock connection and closes it at once), passwordless sudo, and the tmpfiles line for
   `werft.ssh.authorized_keys`.
3. Power off from inside the guest. Undefine the temporary domain, keep the volumes under
   the stock name, write the image record.

Build VMs and `-full` stay without passwordless sudo, as today. Only stock images get the seal
changes. Seal removes no secrets: a `full` lease holds them by design
([PRODUCT.md](PRODUCT.md#access-and-trust)).

## Provision pipeline

### Step model

`prepare` and `provision` are lists of steps run by `internal/pipeline`. A step has:

- a **check**, run in preflight before any step runs: validate inputs, ask every question, declare
  secrets. So all questions come before any destructive action.
- a **run**.
- an optional **cleanup**, pushed on a stack before the run starts, so a half-done step is undone.
  On success, failure and SIGINT the runner pops the stack in reverse. Each cleanup must be safe
  to run twice. `undo` takes a cleanup off the stack only when it succeeds, so a failed one is
  tried again at the end. The `viewer` cleanup runs only through `undo` and after a successful
  run; a failed run leaves the window open on the VM it leaves running.

The runner catches SIGINT and SIGTERM only after preflight and the secrets. An interrupt during a
question, a secret prompt or `pass` ends werft before any change. After that, the runner checks
for an interrupt before each step.

Steps share one run context (domain, address, guest family, secrets, log writer). On failure the
runner stops, leaves the VM running, and prints the SSH command and log path. The log file gets
plain text without ANSI codes.

### Built-in steps

All generic.

| Step | Does |
|---|---|
| `reset` | Force off the VM and revert it to a reset point |
| `boot` | Start the VM; type the disk password when the LUKS marker is set |
| `viewer` | Open a viewer; `undo` or a successful run closes it, a failed run leaves it open |
| `wait-ssh` | Wait until sshd answers; `prepare` also installs the werft key |
| `verify-guest-password` | Check the guest password with one `sudo` call |
| `detect-guest` | Detect the family (`ubuntu` or `omarchy`) and the session process |
| `hostname` | Set the guest hostname to the domain name |
| `luks-mark` | Probe the root disk for LUKS and set the LUKS marker |
| `autologin` | Turn GDM autologin on, and off again in cleanup; `prepare` checks it is off |
| `upload` | Copy the embedded helpers and the recipe files into the guest |
| `wait-session` | Wait for the desktop session |
| `unlock-keyring` | Unlock the GNOME keyring |
| `sudo-grant` | Grant passwordless sudo, and revoke it in cleanup |
| `usb-passthrough` | Attach a host USB stick, and release it in cleanup |
| `pinentry` | Install a pinentry stub with the passphrase in tmpfs and a gpg-agent line; remove them in cleanup |
| `run` | Run a guest command or script, with an optional secret on stdin |
| `checks` | Copy a host check directory into the guest and run its entry script; a failure is `checks_failed` |
| `reboot`, `poweroff` | Power off from inside the guest; `reboot` then boots and waits for SSH |
| `clone` | Delete the old clone, define a new one from generated XML, copy the viewer settings; the steps after it act on the clone |
| `generalize` | Boot the clone, give it a new machine-id, new SSH host keys and its hostname, power it off |
| `snapshot` | Take a reset point |
| `undo` | Run the cleanup of an earlier step now, so a temporary change ends where the recipe says |

`prepare` is a fixed built-in step list; it needs no recipe. Its two waits for a person (sshd install,
disk password) stay, with a hint for each.

The clone gets a copy of each writable disk and of the NVRAM, made with `StorageVolCreateXMLFrom`
in the pool of the original (the NVRAM in the `nvram` pool). An NVRAM template does not work:
firmware autoselection refuses a qcow2 template. A read-only or shareable disk, such as an
installer ISO, stays shared. Preflight finds the volume of each writable disk and of the NVRAM; one
outside every storage pool fails there, before anything changes. A copy that fails deletes the
copies made so far. Before `clone` removes the old `<name>-full`, it checks again that it is shut
off. It deletes the old NVRAM through the storage API: an undefine that deletes the NVRAM unlinks
the file, but its pool keeps the name, and the new copy under that name fails.

The guest login is `$USER` with the werft key and `~/.ssh/id_ed25519`, through the werft
`ssh_config`. It sets `IdentitiesOnly`, so no other agent key uses up sshd's `MaxAuthTries`.
`prepare` checks the werft key alone (`ssh -P werft-key-only`, which the config maps to that key).
When the guest refuses it, `prepare` adds the key over a login with the user's key, else with
`ssh-copy-id`, which asks for the password. The address is the vsock CID when the domain has one, else the DHCP lease of each MAC;
the first that answers SSH wins. A cleanup reaches its domain again by the same rule, because the
guest can reboot and the run can move on to the clone after the step ran.

### Recipe

A recipe is a TOML file that lists the provision steps and their parameters. The setup repo owns
it and the files it names; paths resolve relative to the recipe, and `~/` is the home directory. A
step can be limited to a guest family. `config.toml` maps each build VM to its recipe (`recipe` for
all, `[recipes]` per build VM); `--recipe` overrides it. `prepare` reads only `[guest_password]`
from it.

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
`family` before `detect-guest`, `generalize` before `clone`, and an `undo` without an earlier step
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
- JSON holds no secret. It gives the path of the lease private key.

Exit codes are part of the contract:

| Code | Kind | Example |
|---|---|---|
| 0 | `ok` | |
| 1 | `failed` | unexpected libvirt or SSH error |
| 2 | `usage` | unknown flag, missing argument |
| 3 | `not_found` | no such domain, image, lease or reset point |
| 4 | `conflict` | VM not shut off, label exists without `--replace` |
| 5 | `needs_confirmation` | no TTY and no `--yes` |
| 6 | `no_capacity` | lease limit, pool space or memory |
| 7 | `timeout` | no SSH, no lease address, no shutdown in time |
| 8 | `checks_failed` | the provision checks failed |
| 9 | `unsupported` | raw-NVRAM guard, libvirt version |
| 10 | `secret_unavailable` | `pass` failed and no TTY |

## Concurrency and locking

- libvirt is the arbiter. A unique domain name reserves a lease. QEMU image locks prevent two
  writers on one disk. Stock files are opened read-only as backing files.
- `locks/global.lock` (`flock`) serializes admission, seal, reap, and stock garbage collection.
  Admission holds it from the capacity check until the lease domain is defined. It is held for
  seconds, never for a boot.
- `locks/<domain>.lock` serializes `snapshot`, `reset`, `prepare`, `provision`, `seal` and
  `migrate` on one build VM. A second run fails with `conflict` and does not wait.
- Leases run in parallel. Limits: the memory budget, pool free space, and the 253 addresses of the
  NAT range.
- A stock version is deleted only when no lease domain names it in its metadata.

## Testing

The reset-point design depends on a live test that a revert of a shut-off domain restores a changed
UEFI variable. It exists and passes; see [TESTING.md](TESTING.md).

## Migration from `vm`

The phased plan is in [PRODUCT.md](PRODUCT.md#roadmap). Technical constraints:

- The user migrates one build VM at a time with `werft migrate`. `vm` keeps working on the domains
  not yet migrated and refuses a migrated domain.
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
  explicit loader.
- **systemd versions.** vsock SSH and `ssh.ephemeral-authorized_keys-all` need systemd 256.
  Ubuntu 24.04 has 255 and uses the TCP fallback. A static `/etc/hostname` overrides the hostname
  credential, so seal must delete it.
- **Duplicate DHCP DUID.** Clones with one machine-id get one IP. The machine-id credential
  prevents it only if seal empties `/etc/machine-id`.
- **Desktop internals.** Keyring unlock uses gnome-keyring's
  `InternalUnsupportedGuiltRiddenInterface`, which can change between releases.

## Open questions

- **Legacy labels on migration.** Keep only the kept label, or export `clean-install` to a flat
  file? `clean-install` is never used by `provision` today.
- **Reaping without a command.** Also a systemd user timer that runs `werft reap`?
- **SSH library.** OpenSSH child processes (proposed), or `golang.org/x/crypto/ssh` with an AF_VSOCK
  dialer: no process per call, but a different path from the one the agent uses.
- **Recipe location.** Where in the setup repo the recipe lives. TOML with a `family` key per step
  is enough for the setup recipe.
- **Scripted secrets.** Is `pass` plus TTY enough, or does an unattended `provision` need a secret
  file or file descriptor? Can an agent run `provision` without a TTY at all, given the key stick
  and the `pinentry` secret?
- **Shell completion.** Does werft generate zsh completion, replacing `completion-zsh/vm.rc`?
- **Lease owner.** Where the value comes from: an `--owner` flag, an env var set by the agent
  harness, or `$USER`. Does `release` refuse a lease of another owner?
