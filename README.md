# werft

Disposable test VMs from stock images, on one Linux workstation with libvirt. Built for agents first: get a VM, reach it over SSH, test something in it, give it back.

**Status:** early. Only `werft version` exists. What werft will do is in [docs/PRODUCT.md](docs/PRODUCT.md), how it will work in [docs/DESIGN.md](docs/DESIGN.md).

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

## License

Private. All rights reserved.
