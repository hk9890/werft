# Contributing to werft

This file is the entry point for human contributors. It points to the doc that owns each step.
AI agents start from [AGENTS.md](AGENTS.md).

## Getting started

1. Build from source: see [docs/CODING.md](docs/CODING.md#build-and-run-from-source).
2. Learn where code goes and what a change must never do to a build VM: see
   [docs/CODING.md](docs/CODING.md).
3. Run the tests and learn the layers: see [docs/TESTING.md](docs/TESTING.md).
4. Run werft by hand without touching your own VMs: see [docs/RUNNING.md](docs/RUNNING.md).

## Before you open a PR

Follow [the commit rules](docs/CHANGE-WORKFLOW.md#commits) and
[the checks before push](docs/CHANGE-WORKFLOW.md#before-push) of `docs/CHANGE-WORKFLOW.md`, and
the rules in [docs/REVIEWING.md](docs/REVIEWING.md).

The skills that the docs name, such as `tasks:tasks-core`, come from the
[claude-dev-tools](https://github.com/hk9890/claude-dev-tools) marketplace. You need them only when
an agent does the work.

## Where things live

- Using werft: [README.md](README.md)
- Finding your way around the source: [docs/OVERVIEW.md](docs/OVERVIEW.md)
- Cutting a release: [docs/RELEASING.md](docs/RELEASING.md)
- Reporting a vulnerability: [SECURITY.md](SECURITY.md)

Report a bug or propose a change in a [GitHub issue](https://github.com/hk9890/werft/issues). Open
an issue before a large change, so we can agree on the approach first.
