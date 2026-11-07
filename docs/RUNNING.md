# Running

How an agent builds werft and drives it by hand to reproduce a bug or verify a change. For the
automated suites see [TESTING.md](TESTING.md); for the commands and their flags see
[README.md](../README.md) and `werft help <command>`.

Call the Skill tool with `run` for the generic launch-and-drive flow. **Local delta:** everything
below; where it conflicts with the `run` flow, this file wins.

## Build and launch

Build `./bin/werft` as [CODING.md, "Build and run from source"](CODING.md#build-and-run-from-source)
says, then check the build:

```sh
./bin/werft version
```

- Run `./bin/werft` from the repo root. A `werft` on `PATH` is an older build.
- `mise run werft -- <command>` builds from the current source, then runs it in your shell's
  directory. Its stdout is the same as that of `./bin/werft`; mise writes its own line to stderr.
- Add `--json` to parse a result: one JSON document on stdout, progress and logs on stderr.
- Read the exit code; the codes and the error document are in [DESIGN.md](DESIGN.md), "Agent interface".
- Pass `--yes` to every command that asks; without a terminal it fails with `needs_confirmation`.

## What is safe on this host

The host is the user's workstation. Its build VMs are every domain not named `werft-test-*` (see
`werft list`), and they are the user's work.

- Run `list`, `snapshots`, `help` and `version` at any time. They change nothing.
- `lease`, `leases`, `extend`, `release` and `reap` first release the expired leases and the
  orphans of the state directory they run with
  ([DESIGN.md, "Lease lifecycle"](DESIGN.md#lease-lifecycle)). With your real state directory they
  can release a lease of another agent that expired; run them with an isolated state directory
  ([Isolate config and state](#isolate-config-and-state)).
- Run `start`, `stop`, `snapshot`, `reset`, `migrate`, `prepare` and `provision` only on a domain
  named `werft-test-*`.
- Run `images --prune` only with an isolated state directory. With your real one it deletes the
  user's old stock versions.
- Run a write command on a build VM only when the user is present and asked for that run.

## Get a scratch domain

werft has no command that creates a domain. Make one through `internal/livetest`:

1. Do the one-time image setup in [TESTING.md](TESTING.md), "Live test setup".
2. Write a throwaway `*_live_test.go` (build tag `live`) in `cmd/werft` that calls
   `livetest.NewGuest(t, h, "werft-test-<track>-<name>")` and drives the CLI with `werftJSON`,
   as `TestLifecycleLive` in `cmd/werft/lifecycle_live_test.go` does.
3. Run it alone: `go test -count=1 -tags=live -run <TestName> ./cmd/werft`.
4. Delete the file before you commit.

`NewGuest` removes the domain and its volumes when the test ends. To inspect it after a failure,
set `WERFT_TEST_KEEP=1`; [TESTING.md](TESTING.md), "Debug a failing live test", has the SSH command.

## Isolate config and state

`migrate` writes its exports into the pool that `config.toml` names (default `werft`). Point a
scratch run at the `werft-test` pool and a throwaway state directory:

```sh
export XDG_CONFIG_HOME=$(mktemp -d) XDG_STATE_HOME=$(mktemp -d)
mkdir -p "$XDG_CONFIG_HOME/werft"
echo 'pool = "werft-test"' > "$XDG_CONFIG_HOME/werft/config.toml"
```

`isolate` in `cmd/werft/resetpoint_live_test.go` does the same inside a test.

- The real config is `~/.config/werft/config.toml`; read it, and leave it as the user wrote it.
- The provision recipe and its config keys are in [examples/setup/README.md](../examples/setup/README.md).
