# Setup recipe

`recipe.toml` is the `werft provision` recipe for the setup repo
([carlo9890/setup](https://github.com/carlo9890/setup)). It does what `vm provision` does: reset
the build VM to `provision-base`, run [bootstrap-scripts](https://github.com/carlo9890/bootstrap-scripts)
`install.sh` with the key stick passed through, reboot, run `~/setup/tests/system/run.sh`, and keep
the result as `<build VM>-full` with the reset point `clean`. `bootstrap.sh` is the guest script
that clones bootstrap-scripts and runs it. The recipe lives here until the setup repo takes it over.

## Use it

Point `~/.config/werft/config.toml` at the recipe, for every build VM or for one:

```toml
recipe = "~/dev/github/werft/examples/setup/recipe.toml"

[recipes]
omarchy-4-0 = "~/dev/github/werft/examples/setup/recipe.toml"
```

Then, once per build VM and again after a change to the base:

```sh
werft prepare ubuntu-26-04      # werft key, hostname, provision-base
werft provision ubuntu-26-04    # or: werft provision --recipe <file> ubuntu-26-04
```

`werft provision` needs:

- the setup checkout at `~/setup`, for `tests/system`;
- the key stick `MASTER-KEY` mounted on the host, with no program that holds it;
- a terminal, for the GPG passphrase of the key on the stick;
- the guest password in `pass` entry `vm/guest-password`, else it asks at the terminal.

A run logs to `~/.local/state/werft/logs/provision-<name>-<YYYYmmdd-HHMMSS>.log`. A failed run
leaves the build VM running and prints the `ssh` command that reaches it.

## Differences from `vm provision`

- The base must carry the werft key. Run `werft prepare` once on a build VM that `vm prepare` made.
- Reset points are native libvirt snapshots. Run `werft migrate <build VM>` first.
- The stick has no `--key` flag. The recipe names it by its label, under `/media/$USER` or
  `/run/media/$USER`.
- Secrets come from `pass` or the terminal, not from stdin lines.
- The pinentry stub is a werft step, not part of `bootstrap.sh`.
