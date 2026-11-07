# Changelog

What changed in each release, written for whoever runs `werft`. This is the
history readable from a checkout with no network.

Each section is also the text of its release on the
[GitHub Releases](https://github.com/hk9890/werft/releases) page.

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
