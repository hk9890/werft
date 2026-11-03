# AGENTS.md — werft routing

## Repository purpose

werft is a Go CLI that hands out disposable test VMs from stock images on one libvirt host, for agents first and humans second. It replaces the bash tool `vm` of the setup repo. The host is the user's workstation, and its build VMs are the user's work.

## Use-case routing

Every route below is **mandatory, not advisory**. Load the document BEFORE the first action of that kind — loading it afterwards does not count, and no route becomes skippable because the task looks small.

### Research, planning, analysis

**MUST read [docs/OVERVIEW.md](docs/OVERVIEW.md) before searching this repository.** It is the map — package layout, state locations, and the `rg` expressions that find commands, steps and error kinds.

### Product scope and features

**MUST read [docs/PRODUCT.md](docs/PRODUCT.md) before planning a feature, adding a command, or changing what a user or agent sees.** It owns who werft serves, the core concepts, the agent contract, and the roadmap.

### Architecture and new mechanisms

**MUST read [docs/DESIGN.md](docs/DESIGN.md) before changing the architecture or adding a mechanism.** It owns the package roles, the libvirt and SSH boundaries, the lease and reset-point mechanics, and the exit-code contract.

### Coding and file changes

**MUST read [docs/CODING.md](docs/CODING.md) before creating or editing ANY file outside the Markdown docs.** It owns the build commands, where code goes, and the mechanisms a change must use.

### Testing and verification

**MUST read [docs/TESTING.md](docs/TESTING.md) before writing a test** or judging whether a change is verified. It owns the live layer against `qemu:///system`, what a run creates, and the checks a PR needs.

### Release

**MUST read [docs/RELEASING.md](docs/RELEASING.md) before cutting a release or changing `.goreleaser.yaml` or `.github/workflows/release.yml`.** It owns the tag, the dispatch, the verification, and the install through mise.

### Run werft by hand

**MUST read [docs/RUNNING.md](docs/RUNNING.md) before running `./bin/werft` or any werft command against `qemu:///system`.** It owns which commands are safe on this host and how to get a scratch domain.

### Analyze logs and evidence

**MUST read [docs/MONITORING.md](docs/MONITORING.md) before reading a provision log, a lock, a console log, or libvirt state.** It owns where the evidence of a past run lands and how to read it without root.

### Commit, branch, PR workflow, session close

**MUST read [docs/CHANGE-WORKFLOW.md](docs/CHANGE-WORKFLOW.md) before ANY git operation** — commit, branch, worktree, push, or PR — and again before treating a session as finished. It owns the branch and worktree rule, the commit format, the pre-push gate, the merge steps, and the session-close gate.

### Reviewing changes

**MUST read [docs/REVIEWING.md](docs/REVIEWING.md) before reviewing a PR or a diff.** It carries the host-safety, agent-contract and parity checks the generic review skills cannot know.

### Writing project docs

**MUST read [docs/DOCUMENTING.md](docs/DOCUMENTING.md) and call the Skill tool with `instruction-writing:writing-project-docs` before creating or editing ANY Markdown file in this repository.** The skill carries the file-ownership contract; DOCUMENTING.md carries the local delta and what werft decided not to document.

### Issue tracking

**MUST call the Skill tool with `tasks:tasks-core` and read [docs/CHANGE-WORKFLOW.md](docs/CHANGE-WORKFLOW.md#issues) before filing, claiming, or closing an issue.**
