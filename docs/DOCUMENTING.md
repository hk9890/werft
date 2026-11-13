# Documenting

How werft writes its own docs. For the generic standard, call the Skill tool with
`instruction-writing:writing-project-docs`. This file holds only the local delta. Where the two
conflict, this file wins.

## Language and citation

- Write in ASD-STE100 Simplified Technical English: short sentences, one instruction per line, active voice.
- Cite code by path and symbol, for example `internal/virt/libvirt/uefi_reset_live_test.go`
  and `TestUEFIResetPointRestoresNVRAMAndDisk`. Do not cite a line number.
- Cite a command as a mise task (`mise run test:live`) when `.mise.toml` has one.

## Docs outside the canonical set

- [PRODUCT.md](PRODUCT.md) owns what werft does and for whom: users, core concepts, the agent
  contract, the roadmap, and the open product questions.
- [DESIGN.md](DESIGN.md) owns how werft is built: packages, data model, lease and reset-point
  mechanics, the provision pipeline, exit codes, risks, and the open technical questions.
- [CHANGELOG.md](../CHANGELOG.md) owns what changed for the user in each release; its section is
  the release text on GitHub. [RELEASING.md](RELEASING.md#what-goes-in-the-changelog-section)
  owns the rules for writing it.

## Decisions and work items

- Record a decision in the section of PRODUCT.md or DESIGN.md that owns its subject.
- Mark a row of the DESIGN.md "Proposals in short" table `(accepted)` when the user accepts it.
- Close an open question: write the answer into the owning section, then delete the question from
  "Open questions".
- Track a work item, a plan step or a TODO as an issue ([CHANGE-WORKFLOW.md](CHANGE-WORKFLOW.md#issues)).
  The docs hold the target state, not the backlog.

## What werft does not document, and why

Re-open a decision here before you fill the gap in another file.

- **No CONTRIBUTING.md.** werft has one user, and agents do the work. The human entry point is
  [README.md](../README.md); the agent entry point is `AGENTS.md`.

## Before you land a doc change

No tool checks the docs, and CI (`mise run quality`) does not read Markdown. Check by hand:

1. Every path you wrote exists in the tree: `ls <path>`.
2. Every command you wrote runs, or is a task in `.mise.toml`: `mise tasks`.
3. Every symbol you cited exists: `rg -n '<symbol>' cmd internal`.
4. Every relative link and `#anchor` resolves to a file and a heading.
