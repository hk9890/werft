# Overview

werft is one Go binary: entry point in `cmd/werft`, all policy in `internal/`. The role of each
package and the port-and-adapter shape are in [DESIGN.md, "Packages"](DESIGN.md#packages).

## Structure

```
cmd/werft/                 entry point and command handlers
internal/virt/             hypervisor port
internal/virt/libvirt/     go-libvirt adapter of the port
internal/virttest/         fake hypervisor for unit tests
internal/domxml/           read, build and edit domain XML (libvirtxml)
internal/lifecycle/        list, start, stop
internal/resetpoint/       snapshot, snapshots, reset, migrate, raw-NVRAM guard
internal/image/            seal and the stock image records (images.toml)
internal/lease/            lease and release
internal/pipeline/         step runner for prepare and provision
internal/steps/            built-in steps and their assembly
internal/recipe/           recipe TOML load and validation
internal/guest/            guest port, OpenSSH adapter, embedded guest scripts
internal/console/          boot-console typing: screenshot, key codes
internal/secret/           secrets from pass or a TTY prompt
internal/hostdev/          host side of USB passthrough
internal/config/           ~/.config/werft/config.toml
internal/state/            paths and locks under ~/.local/state/werft/
internal/failure/          error kinds
internal/ansi/             strip terminal escapes for the provision log
internal/build/            version stamp (ldflags)
internal/livetest/         helpers for live tests (build tag live)
scripts/release-notes      prints a CHANGELOG.md section as the release text
```

Where werft keeps state on disk and in libvirt: [DESIGN.md, "Where state lives"](DESIGN.md#where-state-lives).

## Finding things

```bash
rg -n 'name: "' cmd/werft/cli.go                          # every command, its flags and summary
rg -n 'func \(c \*cli\) cmd' cmd/werft                     # the handler of each command
rg -n 'case "' internal/steps/recipe.go                    # every recipe step type and its Go type
rg -n 'Name: "' internal/steps/prepare.go                  # the fixed step list of prepare
rg -n 'Run\(r \*pipeline\.Run\) error' internal/steps      # every step implementation
rg -n 'failure\.NotFound' cmd internal                     # where one error kind is raised or tested
rg -l '^//go:build live' cmd internal                      # every live test file
rg -n 'werft-test-' --glob '*_live_test.go'                # the scratch domains a live test makes
rg -n '^\s+\w+\(' internal/virt/virt.go                    # every call of the hypervisor port
```

## External resources

| Resource | URL |
|----------|-----|
| `vm`, the bash tool werft replaces | https://github.com/carlo9890/setup/tree/main/scripts/vm |
| The provision recipe of the setup repo | https://github.com/carlo9890/setup/tree/main/tests/werft |
| libvirt API reference | https://libvirt.org/html/index.html |
| libvirt domain XML format | https://libvirt.org/formatdomain.html |
| go-libvirt (RPC client) | https://pkg.go.dev/github.com/digitalocean/go-libvirt |
| libvirtxml (XML types) | https://pkg.go.dev/libvirt.org/go/libvirtxml |
| systemd credentials (SMBIOS type 11) | https://systemd.io/CREDENTIALS/ |
