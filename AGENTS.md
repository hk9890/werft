# AGENTS.md — werft routing

## Repository purpose

werft is a Go CLI that hands out disposable test VMs from stock images on one libvirt host, for agents first and humans second. It replaces the bash tool `vm` in the setup repo. The foundation packages, `werft version`, `list`, `start`, `stop`, `prepare` and `provision` exist in code; the docs below hold the target.

## Use-case routing

Every route below is **mandatory, not advisory**. Load the document BEFORE the first action of that kind — loading it afterwards does not count, and no route becomes skippable because the task looks small.

### Product scope and features

**MUST read [docs/PRODUCT.md](docs/PRODUCT.md) before planning a feature, adding a command, or changing what a user or agent sees.** It owns who werft serves, the core concepts, the agent contract, the roadmap, and the open product questions.

### Architecture and implementation

**MUST read [docs/DESIGN.md](docs/DESIGN.md) before creating or editing ANY file under `cmd/` or `internal/`.** It owns the package layout, the libvirt and SSH boundaries, the lease and reset-point mechanics, and the exit-code contract.

### Testing and verification

**MUST read [docs/TESTING.md](docs/TESTING.md) before writing a test** or judging whether a change is verified. It owns the live layer against `qemu:///system`, what a run creates, and the checks a PR needs.

### Writing project docs

**MUST call the Skill tool with `instruction-writing:writing-project-docs` before creating or editing ANY Markdown file in this repository.** It carries the file-ownership contract.

### Commit, branch, PR workflow

**MUST call the Skill tool with `commit-commands:commit` before committing.** Run `mise run check` first; CI runs the same task.
