# Coding

What a change to werft needs in order to work. The package map is in
[DESIGN.md, "Packages"](DESIGN.md#packages); this file says which mechanism a change must use.

## Build and run from source

```sh
mise run build                  # bin/werft, with the version stamp of internal/build
mise run werft -- <command>     # build, then run in your shell's directory; version prints "dev"
```

- Keep a source run away from your locks, werft key and `config.toml` as
  [RUNNING.md, "Isolate config and state"](RUNNING.md#isolate-config-and-state) says.
- The gate before a PR is in [TESTING.md](TESTING.md#minimum-checks-before-opening-a-pr).

## Act only on test domains

- Run werft by hand only as [RUNNING.md, "What is safe on this host"](RUNNING.md#what-is-safe-on-this-host)
  allows.
- Keep every command acting only on the domain named on its command line, plus its `-full` clone
  (`buildVMRun` in `cmd/werft/adapters.go`, `resetpoint.FullSuffix`). No code path picks a domain
  by itself.
- In unit tests, set `cli.dial` to a `virttest.New()` fake (`cmd/werft/lifecycle_test.go`); step
  tests use the fake world in `internal/steps/harness_test.go`.
- In live tests, create and remove domains as
  [TESTING.md, "Write a live test"](TESTING.md#write-a-live-test) says.
- Keep host code free of sudo. werft reaches `qemu:///system` as the user, through group
  `libvirt`; host tools in `internal/hostdev` run unprivileged.

## Where code goes

- **Commands**: `cmd/werft`, one file per command group (`lifecycle.go`: `list`, `start`, `stop`;
  `resetpoint.go`: `snapshot`, `snapshots`, `reset`, `migrate`; `provision.go`: `prepare`,
  `provision`; `image.go`: `seal`, `images`; `lease.go`: `lease`, `release`, `leases`, `extend`, `reap`; `guide.go`: `guide`). Flags, rendering and confirmation only; policy goes in `internal/`.
- **Real adapters**: build them only in `cmd/werft/main.go` and `cmd/werft/adapters.go`
  (`dialLibvirt`, `buildVMRun`). Every other package takes the interfaces.
- **libvirt**: call it only through `virt.Hypervisor` (`internal/virt/virt.go`). For a new call,
  add the method to the interface, implement it in `internal/virt/libvirt/adapter.go` and in
  `internal/virttest/fake.go`. Code never runs `virsh`.
- **Domain XML**: read and edit it with the pure functions of `internal/domxml` (libvirtxml).
- **Guest**: reach it only through `guest.Shell` (`internal/guest/guest.go`). In a step, use
  `r.Exec`, `r.Output` and `r.Root`. Pass a secret on stdin with `guest.Line`, never as an
  argument.
- **Errors**: create them with `failure.New` or `failure.Wrap` and a `failure.Kind`. `exitCodes`
  in `cmd/werft/cli.go` maps the kind to the exit code. A new kind needs an entry there and a row
  in [DESIGN.md, "Agent interface"](DESIGN.md#agent-interface).
- **Output**: write the result with `c.output(v, text)`, which prints `v` as JSON under `--json`
  and calls `text` otherwise. Send progress to stderr. `cli.run` reports errors; return them.
- **Confirmation**: call `c.confirm(yes, question)` before a destructive action, with `--yes` and
  `-y` flags (model: `cmdReset` in `cmd/werft/resetpoint.go`). Without a terminal it fails with
  `needs_confirmation`.
- **Locks**: take `LockDomain` from `state.Root()` before a command changes a build VM;
  `c.lockedHypervisor(name)` in `cmd/werft/resetpoint.go` does both. A held lock fails with
  `conflict`.
- **Setup recipe**: keep `examples/setup/recipe.toml` and `examples/setup/bootstrap.sh` as
  [examples/setup/README.md](../examples/setup/README.md) describes them; change the README in the
  same PR.
- **Guest assets**: put the file in `internal/guest/assets/`, add a `//go:embed` variable in
  `assets.go`, and add it to `Helpers()` when the `upload` step must copy it.

## Add a command

1. Write `func (c *cli) cmdName(args []string) error` in the file of its group. Model: `cmdSnapshot`
   in `cmd/werft/resetpoint.go`.
2. Parse flags with `flag.NewFlagSet(name, flag.ContinueOnError)` and `c.parseFlags`, so `-h` and
   `--json` help work.
3. Return `failure.Usage` for wrong arguments.
4. Put the logic in an `internal/` package that takes a `virt.Hypervisor`.
5. Register the command in `commands` in `cmd/werft/cli.go` (`init`), with `name`, `args` and
   `summary`; `werft help` and `werft completion zsh` read it. A new placeholder in `args` needs
   an entry in `completers` (`cmd/werft/completion.go`).
6. Test it against the fake through `cli.dial` (`cmd/werft/lifecycle_test.go`).

## Add a provision step

1. Write the type in `internal/steps`: `guest.go` for work in the guest, `host.go` for the host,
   `boot.go` for power, `clone.go` for clone and snapshot. Model: `Hostname` in `guest.go`.
2. Implement `pipeline.Step` (`internal/pipeline/pipeline.go`). `Check` changes nothing: it
   validates, queues questions with `r.Ask` and declares secrets with `r.Declare`. `Run` does the
   work.
3. Implement `pipeline.Cleaner` when the step changes something to undo; `Cleanup` must be safe to
   run twice and after a half-done run. Model: `SudoGrant` in `guest.go`. Add `KeepOnFailure` to
   keep the change after a failed run (model: `Viewer` in `host.go`).
4. Add the step type and its keys to `params` in `internal/recipe/recipe.go`.
5. Map the type in `steps.FromRecipe` (`internal/steps/recipe.go`). A `prepare` step goes in the
   fixed list `steps.Prepare` (`internal/steps/prepare.go`).
6. Add a row to [DESIGN.md, "Built-in steps"](DESIGN.md#built-in-steps).
