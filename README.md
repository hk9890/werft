# werft

Disposable test VMs from stock images, on one Linux workstation with libvirt. Built for agents first: get a VM, reach it over SSH, test something in it, give it back.

The status of each phase is in the [roadmap](docs/PRODUCT.md#roadmap).

## Requirements

- Linux with libvirt 11.4 or later (`qemu:///system`), and the user in group `libvirt`
- OpenSSH client; `virt-viewer` for `--view`

## Install

Install a release with mise. The repository is private, so mise needs a GitHub token with the
`repo` scope, or Contents: read for a fine-grained token. mise takes the first of `MISE_GITHUB_TOKEN`, `GITHUB_API_TOKEN`, `GITHUB_TOKEN` and
the gh CLI login; `mise token github` shows which one it uses. [CHANGELOG.md](CHANGELOG.md) says
what changed in each release.

```sh
mise use -g github:hk9890/werft@X.Y.Z    # a version from the releases page
```

To build from source, follow [docs/CODING.md](docs/CODING.md#build-and-run-from-source).

## Usage

Every command takes `--json` and then prints one JSON document on stdout. `werft help <command>`
shows the flags; `werft help`, or `werft` with no command, lists the exit codes.

For zsh completion of commands, VM names, reset points, images and lease ids, add
`source <(werft completion zsh)` to `~/.zshrc`.

### Build VMs

```sh
werft list                                   # every VM: state, disk use, reset points, address
werft start ubuntu-26-04                     # headless; waits up to 60 s for the DHCP address
werft start --view ubuntu-26-04              # also opens virt-viewer (Shift+F11 fullscreen, Shift+F12 release cursor)
werft stop ubuntu-26-04                      # ACPI shutdown, waits up to 120 s
werft stop --force ubuntu-26-04              # forces the VM off at once
```

### Reset points

A reset point holds disk and UEFI variables of a shut-off VM. The default label is
`clean-install`.

```sh
werft migrate ubuntu-26-04                   # once per build VM that vm manages; asks first
werft snapshot ubuntu-26-04                  # take clean-install
werft snapshot --replace ubuntu-26-04 mine   # retake the label mine
werft snapshots ubuntu-26-04                 # list them; also works while the VM runs
werft reset ubuntu-26-04 mine                # roll back; asks first, --yes without a terminal
```

`snapshot` and `reset` refuse a running VM; stop it first. Until `werft migrate` ran, they refuse a
VM whose UEFI variables are raw, because a reset point would leave them out.

### Provision a build VM

`werft provision` runs a recipe on a build VM and keeps the result as `<name>-full`. Point
`~/.config/werft/config.toml` at a recipe with `recipe = "<path>"`, or pass `--recipe <path>`.
[examples/setup/README.md](examples/setup/README.md) holds the recipe of the setup repo, its
config, and what a run needs.

```sh
werft prepare ubuntu-26-04                   # once: werft key and your own key, hostname, provision-base
werft provision ubuntu-26-04                 # reset, run the recipe, clone to ubuntu-26-04-full
werft provision --key /media/$USER/BACKUP ubuntu-26-04  # the key stick at another mount point
```

### Stock images

A stock image is a sealed copy of a build VM reset point. Leases start from it.

```sh
werft seal ubuntu-26-04 provision-base       # reset to provision-base, seal a copy as the plain tier; asks first
werft seal ubuntu-26-04-full clean           # reset to clean, seal a copy as the full tier of ubuntu-26-04; asks first
werft images                                 # every stock version: image, tier, version, date
werft images --prune                         # delete the old versions that no lease uses; asks first
```

`seal` needs a guest with systemd 256 or later: it logs in as root over vsock. It refuses an
older guest at once, when the reset point records its systemd version (`werft prepare` and the
`detect-guest` step write it). A reset point without that record seals as before; when the vsock
login then times out, the error tells you to run `werft prepare <vm>` and take the reset point
again.

To refresh both tiers after a change to the base, run these in order. New leases take the newest
version; old versions stay until `werft images --prune`. Prune keeps the newest version of each
image and tier, and every version a lease uses.

```sh
werft reset ubuntu-26-04 provision-base      # back to the base
werft start ubuntu-26-04                     # make the change in the guest, then:
werft stop ubuntu-26-04
werft snapshot --replace ubuntu-26-04 provision-base
werft seal ubuntu-26-04 provision-base       # new plain version
werft provision ubuntu-26-04                 # new ubuntu-26-04-full with the reset point clean
werft seal ubuntu-26-04-full clean           # new full version
```

### Leases

A lease is a disposable VM on a thin overlay of the newest stock version.

```sh
werft lease ubuntu-26-04                     # boot a plain lease; prints its id and ssh command when SSH answers
werft lease ubuntu-26-04 --tier full         # boot a lease of the full tier
werft lease ubuntu-26-04 --owner agent-7     # label the lease (default $WERFT_OWNER, else $USER)
ssh -F ~/.local/state/werft/leases/<id>/ssh_config <id>
werft release <id>                           # destroy the VM, its disk and its key; a second release exits 0
werft leases                                 # id, owner, image, tier, state, address, expiry of each lease
werft extend <id> --for 1h                   # move the expiry by 1h (default 4h)
werft reap                                   # release expired leases and orphans now
werft guide                                  # how an agent leases, uses and releases a VM
```

Point an agent at `werft guide`. It prints the lease flow, the JSON fields, the exit codes and the
rules for an agent, and it matches the installed version.

A lease expires 4 hours after it starts. `lease`, `leases`, `extend`, `release` and
`images --prune` first release the expired leases; `reap` does only that.

Leases run in parallel. `werft lease` exits 6 (`no_capacity`) when the host memory, less the memory
of the running VMs and of every lease, less the new lease, is below `memory_budget_mib` in
`~/.config/werft/config.toml` (default 4096).

A stock image sealed before `werft lease` existed has no domain definition for leases; seal it again.

## Further reading

- [docs/PRODUCT.md](docs/PRODUCT.md): users, core concepts, the agent contract, the roadmap
- [docs/DESIGN.md](docs/DESIGN.md): how werft works, exit codes, where state lives

## License

Private. All rights reserved.
