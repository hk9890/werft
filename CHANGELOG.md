# Changelog

What changed in each release, written for whoever runs `werft`. This is the
history readable from a checkout with no network.

Each section is also the text of its release on the
[GitHub Releases](https://github.com/hk9890/werft/releases) page.

## v0.3.1

- **werft waits longer before it types the disk password of an encrypted
  (LUKS) guest.** It types when the console has held still for 40 s.
  Before, it typed after 8 s, so on the first boot of a copied disk it
  could type into the firmware screen: `provision` then failed with no SSH
  answer while the console still asked for the password, and `seal` and
  `lease` could fail the same way. Each boot of an encrypted guest now
  takes about 30 s longer, and the prompt wait takes up to 3 minutes.
  `werft guide` now asks for a timeout of 20 minutes on a lease call.
- **`werft provision` lets `sudo -v` pass in the guest during the run.**
  Before, a recipe step that ran `sudo -v` stopped with "a terminal is
  required to read the password". Omarchy's update runs it. Ubuntu 26.04
  uses sudo-rs, where `sudo -v` still asks.
- **The example recipe `examples/setup/recipe.toml` runs `omarchy update`
  with `LANG=C.UTF-8`.** Before, the update failed on Omarchy in a
  migration that read a locale warning as its answer.

## v0.3.0

- **`werft images --prune` deletes old stock versions.** It keeps the
  newest version of each image and tier, and every version a lease uses.
  It asks first; `--yes` skips the question. Like `lease`, it first
  releases the expired leases. Before, no command deleted a stock version.
  A new seal never takes the name of a deleted version again.
- **`werft lease` takes `--memory`, `--cpus`, `--for` and `--stock`.**
  `--memory` (MiB, at least 256) and `--cpus` replace the size of the stock
  image. `--for` sets the lifetime, from 15m to 24h; the default stays 4h.
  `--stock <version>` leases that version instead of the newest; without
  `--tier` the lease takes the tier of that version. A `--tier` or image
  that does not match the version fails with exit 2.
- **`werft seal` and `werft lease` work with an encrypted (LUKS) disk.**
  Both type the disk password from `pass` at boot. `werft images` marks
  such a version with `"luks": true`. When the password store is locked,
  the lease fails with `secret_unavailable` and names the `pass` entry to
  unlock. The password never appears in output. This path has no live test
  on a real encrypted guest yet.
- **`werft leases` shows the state of each lease.** The table has a STATE
  column and the JSON a `state` field, such as `running` or `shut off`. A
  lease that is not running shows no address.
- **A lease that does not boot saves its last screen.** The error names the
  file under `~/.local/state/werft/evidence/`. werft keeps the 10 newest.
  A lease stopped by Ctrl-C saves none.
- **`werft seal` refuses a guest with systemd older than 256 before it
  copies a disk.** It fails with `unsupported` when the reset point records
  the systemd version, which `werft prepare` writes. For a reset point
  without that record, a login timeout now tells you to run
  `werft prepare <vm>` and take the reset point again. Before, `seal` copied
  the disk and then failed on the timeout.

## v0.2.0

- **`werft guide` prints how an agent leases a VM, uses it and gives it
  back.** The binary holds the text, so it matches the installed
  version. It gives the real wait of `werft lease` and asks for a 15-minute
  timeout or a background run. It shows how to run commands with ssh and
  `sudo -n`, and how to copy files with tar and scp. It shows a port
  forward that fails when the local port is taken, and how to start a
  server that keeps running after ssh returns. It also covers reboot and
  power off, the tools of the full tier, extend and release, and the exit
  codes. It shows how to find a lost lease by its `--owner`. `werft --json guide`
  prints `{"guide": "<text>"}`.
- **`werft completion zsh` prints a zsh completion script.** Load it with
  `source <(werft completion zsh)`. It completes the commands and their
  flags, build-VM names, reset points, images and lease ids. It does not
  offer lease VMs as build VMs.
- **`werft seal <x>-full clean` makes the full tier.** It writes the image
  version `<x>-full-<YYYYMMDD>` of image `<x>`, so
  `werft lease <x> --tier full` finds a stock image. Before, `seal` made only
  the plain tier and refused every `-full` build VM. `seal <x> provision-base`
  is unchanged.
- **`werft --json list` marks lease VMs.** Each entry has a `lease` field,
  `true` for a VM that a lease made.
- **JSON output writes `&`, `<` and `>` as they are.** Before, a VM name or
  other string with them came out as `\u0026`, `\u003c` and `\u003e`.
- **`werft help` starts every summary in one column.** Before, the summary of
  a command whose name and arguments are wider than the command column
  started further right. It now goes on the next line, indented.

## v0.1.0

- **First release.**
