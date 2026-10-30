# werft

Disposable test VMs from stock images, on one Linux workstation with libvirt. Built for agents first: get a VM, reach it over SSH, test something in it, give it back.

**Status:** phase 1 (parity with `vm`) is in code: `list`, `start`, `stop`, `snapshot`, `snapshots`, `reset`, `migrate`, `prepare` and `provision`. The lease commands do not exist yet. What werft will do is in [docs/PRODUCT.md](docs/PRODUCT.md), how it works in [docs/DESIGN.md](docs/DESIGN.md).

## Requirements

- Linux with libvirt 11.4 or later (`qemu:///system`), and the user in group `libvirt`
- OpenSSH client; `virt-viewer` for `--view`

## Usage

Every command takes `--json` and then prints one JSON document on stdout. `werft help <command>`
shows the flags; `werft help` lists the exit codes.

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
[examples/setup/](examples/setup/README.md) holds the recipe of the setup repo and shows the config.

```sh
werft prepare ubuntu-26-04                   # once: werft key and your own key, hostname, provision-base
werft provision ubuntu-26-04                 # reset, run the recipe, clone to ubuntu-26-04-full
werft provision --key /media/$USER/BACKUP ubuntu-26-04  # the key stick at another mount point
```

### Leases (target)

```sh
werft images --json                          # stock images and tiers
werft lease ubuntu-26-04 --tier plain --json # get a VM; returns when SSH answers
ssh -F <lease ssh_config> <lease-id>         # work in it
werft release <lease-id>                     # destroy it
```

## License

Private. All rights reserved.
