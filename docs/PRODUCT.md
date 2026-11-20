# werft — product

werft (German for shipyard) is a VM lab on one Linux workstation. Agents and humans use it to get a
disposable VM, test something in it, and give it back. Agents are the main users.

werft replaced the bash tool `vm` of the setup repo, the user's own repository that sets up a
fresh machine. The setup repo deleted `scripts/vm/` when werft reached parity.

Technical design: [DESIGN.md](DESIGN.md).

## What werft is

- A command-line tool that manages libvirt VMs on the local host (`qemu:///system`).
- A set of build VMs: vendor installs (for example Ubuntu 24.04, Ubuntu 26.04, Omarchy) with named
  reset points.
- A set of stock images, sealed from build VMs. Leases start from them.
- A way to get a disposable VM in a known state, reach it over SSH, and give it back.
- A way to rehearse a full fresh-machine install end to end, and keep the result as a build VM.

## What werft is not

- Not a cloud. There is no API server, no tenants, no quotas.
- Not multi-host. One workstation, one libvirt instance.
- Not a CI farm. It does not schedule jobs, store artifacts, or report to a forge.
- Not a replacement for libvirt or virt-manager. werft drives libvirt; virt-manager stays the tool
  for manual VM work, and werft VMs stay visible in it.
- Not an image builder. A human installs each build VM by hand from vendor media once; werft takes
  over from there.

## Users and their jobs

Agents come first. The agent commands (`guide`, `images`, `lease`, `extend`, `leases`, `release`, `reap`, `list`,
`start`, `stop`, `reset`, `snapshot`, `snapshots`) work without a TTY and give machine-readable
output. `prepare` and `provision` need a person at the host: `prepare` waits for the sshd install
and the disk password, and `provision` needs the physical key stick and a passphrase typed at a
TTY. That passphrase is never stored, so `provision` keeps that need
([DESIGN.md, "Recipe"](DESIGN.md#recipe)).
Humans come second. They use the same commands and get readable output.

| Job | Who | What they get |
|---|---|---|
| Rehearse a full install | human; an agent can prepare the command | A build VM reset to its `provision-base` point, the bootstrap run in it, checks run, and the result kept as the `-full` build VM. |
| Get a plain VM | agent, human | A lease from a `plain` stock image: the vendor install with SSH access and nothing else installed. |
| Get a fully installed VM | agent, human | A lease from a `full` stock image: the state after the bootstrap, secrets included. |
| Test own software | agent | A lease. The agent installs its software, runs its tests, and gives the lease back. The stock image does not change. |

## Core concepts

- **Build VM.** A libvirt domain that a human installs once from vendor media, for example
  `ubuntu-26-04`, plus its provision result `ubuntu-26-04-full`. `reset` and `provision` change it.
  Leases never run on it.
- **Reset point.** A named, saved state of a build VM: disk and firmware state together. Reset
  returns the build VM to exactly that state. If werft cannot save or restore both halves, the
  command fails. Only build VMs have reset points.
- **Stock image.** A sealed, immutable copy of one build VM reset point. werft never changes it.
  Leases start from it. A new seal makes a new version. `werft images --prune` deletes the old
  versions that no lease uses.
- **Tier.** The state a stock image comes from. `plain` is the vendor install, prepared for SSH
  access. `full` is the state after the full bootstrap and its checks.
- **Lease.** A disposable VM that one user gets from a stock image and tier. The lease has an
  owner, an SSH address, and a lifetime. Release destroys it. Nothing done inside a lease reaches
  the stock image.
- **Provision.** The pipeline that turns a build VM into its `-full` build VM: reset, bootstrap,
  check, clone, generalize, save a reset point.
- **Seal.** The step that makes a stock image from a build VM reset point. It removes the identity
  of the build VM (machine-id, hostname, SSH host keys), so each lease gets its own.

## Access and trust

werft gives an agent no access that the user does not already have. An agent runs as the user on
the host, so it can already read the user's keys, password store and tokens there.

- werft runs as the user, through group `libvirt` on `qemu:///system`. That group is
  root-equivalent on the host; the user is in it already.
- A `full` lease holds the secrets of the installed user: GPG keys, password store, keyrings, the
  `gh` token. This is intended. Release destroys them with the lease.
- The agent logs in as the image's own user, with passwordless sudo.
- A lease uses libvirt's default NAT network. It reaches the internet, the host and the LAN.

## Agent experience

Names and flags can change; the shape is the contract.

The interaction to support: a user tells an agent "get a VM and do some tests in it". The agent
runs:

```sh
werft --json images                                        # which images and tiers exist
werft --json lease ubuntu-26-04 --tier plain --owner <me>  # get a VM; blocks until SSH answers
ssh -F <lease ssh_config> <lease-id> '<command>'           # one command per call; install, test
werft --json release <lease-id>                            # give it back; the VM is destroyed
```

Requirements:

- A Claude Code agent finds werft through the `werft` skill of the `werft` plugin in the
  claude-dev-tools marketplace. When a task needs a VM, the skill loads the output of
  `werft guide`; the guide stays in the werft binary. In the default permission mode, the werft
  and `ssh` calls of the guide run with no permission prompt. When the session ends, the plugin
  releases the leases of that session.
- `werft lease` returns only when the VM accepts an SSH login. The output has the lease ID, the
  address, the SSH user, the key path, the path of the lease `ssh_config`, a ready `ssh` command,
  and the expiry time.
- `--json` gives one JSON document on stdout. Progress and logs go to stderr.
- Exit codes are stable and documented, so an agent can tell "no capacity" (6) from "SSH timeout"
  (7).
- No command asks a question when stdin is not a TTY. A command that needs confirmation fails and
  names the flag that gives it.
- Secrets never appear on a command line, in a process list, or in JSON output.
- A lease lives 4 hours by default; `werft lease --for` sets another lifetime. `werft extend
  <lease-id>` adds time. werft destroys an expired lease at the next werft run that reads leases
  with the same state directory; no timer runs ([DESIGN.md, "Lease lifecycle"](DESIGN.md#lease-lifecycle)
  names those commands). The VM of an agent that crashes stays until then.
- `werft --json release --owner <o>` releases every lease of owner `<o>` in one call, so an agent
  or the end of its session gives back all its leases at once.
- `werft --json leases` lists active leases with owner, image, tier, state, address, and expiry.
  A lease that does not run, such as one the guest powered off, lists its state and no address.
- A lease gets the memory and vCPUs of its stock image by default; `--memory` and `--cpus` set
  others. `--stock` picks an older stock version of the image and tier, for example to bisect.
- More than one lease can run at the same time. werft refuses a new lease ("no capacity") when the
  host memory that no VM and no lease holds would drop below a configured budget.
- A lease runs headless: the guest desktop runs, but no viewer window opens on the host. `--view`
  opens one.
- An agent that tests a desktop app gets a logged-in desktop with no person at the host:
  `werft lease --desktop` logs the image's user in to the desktop and unlocks the login keyring
  with the guest password. werft reads that password as it reads the disk password of an
  encrypted image; the agent never sees it. Without `--desktop`, an Ubuntu lease stops at the
  login screen, and the lease reads no guest password.
- `werft guide` tells an agent with no prior knowledge how to lease, use and release a VM: the
  commands, the lease JSON fields, the exit codes and the rules. `werft help` lists it. It is part
  of the binary, so it matches the installed version.
- A lease of an encrypted image (Omarchy) boots like any other: werft types the disk password. The
  agent never sees it. werft reads it from the user's keyring, else from `pass`, so the keyring or
  the GPG agent of the user must be unlocked; else the lease fails with `secret_unavailable`.
  `seal` and `start` read the password from the same keyring item and `pass` entry. `start` asks
  at the terminal only when every source the recipe names fails.

## What carries over from `vm`

Keep these user-visible behaviors. Each one fixed a real failure in `vm`.

- Every question comes before any destructive step and before any secret is read.
- `provision` checks all its inputs before it stops or resets anything.
- The guest password is checked right after SSH comes up, not at the end.
- Temporary changes in the guest (sudo grant, autologin, pinentry) are removed on success and on
  failure.
- werft types the LUKS disk password once at boot and does not retry.

The implementation invariants behind them are in [DESIGN.md](DESIGN.md#step-model).

Setup-specific parts (the bootstrap repo, the key stick, the GPG passphrase, the system checks,
GNOME autologin, keyring unlock, Omarchy update) move out of the core into a recipe that the setup
repo owns. werft only runs it.

## Roadmap

1. **Parity with `vm`.** Same commands and pipelines in Go: `list`, `start`, `stop`, `snapshot`,
   `snapshots`, `reset`, `prepare`, `provision`, plus `migrate` to move each build VM to werft
   reset points. `vm` and werft ran side by side, one build VM at a time. Status: done; every build
   VM has werft reset points only.
2. **Setup-repo switch.** The setup repo adds its recipe. When `werft provision` makes a `-full`
   build VM that passes the system checks, the setup repo deletes `scripts/vm/`. Status: done; the
   recipe is in `tests/werft/` of the setup repo, and `scripts/vm/` is deleted.
3. **Lease model.** Disposable VMs from a stock image and tier: `seal`, `images`, `lease`, `extend`,
   `release`, `leases`, `reap`, lifetimes, expiry cleanup, parallel leases, stock garbage
   collection (`images --prune`). Status: done, also for encrypted images.
4. **Agent integration.** Stable JSON output and exit codes, a guide that tells an agent how to use
   werft, a way for an agent to find werft, and an end-to-end test where an agent leases a VM, tests
   in it, and releases it. Status: done; the `werft` plugin makes werft known (see "Agent
   experience"). The end-to-end test is a recorded run in [PR #48](https://github.com/hk9890/werft/pull/48). Its prompt was only "Get a VM
   and run curl https://example.com in it.", in the default permission mode, with no tool
   allowed. A fresh `claude -p` session loaded the skill, leased a VM, ran curl in it and
   released it, with no permission prompt.

## Open questions

None open. The technical decisions (lease mechanism, addressing, firmware state, host access) are in
[DESIGN.md](DESIGN.md#proposals-in-short).
