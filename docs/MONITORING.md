# Monitoring

werft has no metrics and no service. The evidence of a past run is in the provision log, the
state directory, libvirt, and the guest consoles. Where each file lives is in
[DESIGN.md, "Where state lives"](DESIGN.md#where-state-lives); this file says how to read it.

Every command below only reads. A command that changes a domain follows
[RUNNING.md, "What is safe on this host"](RUNNING.md#what-is-safe-on-this-host).

## Provision log

`werft provision` writes one log per run (`cmdProvision` in `cmd/werft/provision.go`). `prepare`
writes no log: its output is stderr only.

- Get the path from the `log:` line of the error (`error.message` under `--json`); a successful run
  also returns it in the `log` field.
- List the logs, newest first: `ls -t ~/.local/state/werft/logs/` (`$XDG_STATE_HOME/werft/logs/`
  when that is set, see `state.Root`).
- The log is the stderr of the run as plain text: `ansi.Strip` removes the terminal escapes.
- A failed run ends with the line `werft: <error>`. Read the last lines first:
  `tail -n 40 <log>`.
- A failed `provision` or `prepare` leaves the domain running when it was running at the failure,
  and prints `<domain> is left running for inspection` with the SSH command (`Run.failed` in
  `internal/pipeline/pipeline.go`).

## State directory

- `locks/global.lock`, `locks/<domain>.lock` are `flock` files (`state.Dir.LockGlobal`,
  `state.Dir.LockDomain`). The file stays after a run; it says nothing. A held lock gives
  `another werft run works on <name>`. Find the holder with `lsof ~/.local/state/werft/locks/<name>.lock`.
- `images.toml` exists after the first `seal`; `werft images --json` reads it. `leases/<id>/`
  exists from `werft lease` until the release or the reap of the lease.

## Domain state, reset points, addresses

Use `werft` first; it reads and changes nothing:

```
werft list --json              # every domain: state, disk use, reset points, address
werft snapshots <name> --json  # reset points, those of vm included
```

Use `virsh -r` (read-only connection) for what `werft` does not show:

```
virsh -r -c qemu:///system domstate <domain> --reason
virsh -r -c qemu:///system dumpxml <domain>          # disks, NVRAM, vsock CID, description (LUKS marker)
virsh -r -c qemu:///system snapshot-list <domain>
virsh -r -c qemu:///system net-dhcp-leases default
virsh -r -c qemu:///system vol-list <pool>
```

`snapshot-list` shows libvirt metadata only. A reset point of `vm` has none; `werft snapshots`
shows it.

## Live-test console logs

Each live-test guest writes its serial console to the volume `<domain>-console.log` in pool
`werft-test` (`Guest.ConsoleVolume` in `internal/livetest/livetest.go`). The test deletes it on
success. Keep it after a failure with `WERFT_TEST_KEEP=1` ([TESTING.md](TESTING.md)).

Read it through the pool, with no root:

```
virsh -c qemu:///system pool-refresh werft-test
virsh -c qemu:///system vol-download --pool werft-test <domain>-console.log /tmp/<domain>-console.log
```

Use these only on pool `werft-test` and domains named `werft-test-*`.

## libvirt domain logs

`/var/log/libvirt/qemu/<domain>.log` holds the QEMU command line and QEMU errors. The files are
`0600 root`, and no agent uses sudo. Ask the user to run
`sudo tail -n 50 /var/log/libvirt/qemu/<domain>.log` and paste the output.
