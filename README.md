# werft

Disposable test VMs from stock images, on one Linux workstation with libvirt. Built for agents first: get a VM, reach it over SSH, test something in it, give it back.

**Status:** early. `werft list`, `start`, `stop`, `snapshot`, `snapshots`, `reset`, `migrate`, `prepare` and `provision` exist; the lease commands do not yet. What werft will do is in [docs/PRODUCT.md](docs/PRODUCT.md), how it will work in [docs/DESIGN.md](docs/DESIGN.md).

## Requirements

- Linux with libvirt 11.4 or later (`qemu:///system`), and the user in group `libvirt`
- OpenSSH client

## Usage (target)

```sh
werft images --json                          # stock images and tiers
werft lease ubuntu-26-04 --tier plain --json # get a VM; returns when SSH answers
ssh -F <lease ssh_config> <lease-id>         # work in it
werft release <lease-id>                     # destroy it
```

## Provision a build VM

`werft provision` runs a recipe on a build VM and keeps the result as `<name>-full`. Point
`~/.config/werft/config.toml` at a recipe with `recipe = "<path>"`, or pass `--recipe <path>`.
[examples/setup/](examples/setup/README.md) holds the recipe of the setup repo and shows the config.

```sh
werft prepare ubuntu-26-04                   # once: werft key, hostname, provision-base
werft provision ubuntu-26-04                 # reset, run the recipe, clone to ubuntu-26-04-full
```

## License

Private. All rights reserved.
