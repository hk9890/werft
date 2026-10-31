# Reviewing

The local review rules of werft. Call the Skill tool with `project-review:project-review-change`
for the generic lens; this file holds only the local delta. Where the two conflict, this file wins.

## What to prioritise

- **Build VMs**: a finding under [Host safety](#host-safety) is blocking.
- **Agent contract**: a change that breaks one rule of [Agent contract](#agent-contract) is a
  blocking finding. Agents parse the output and the exit code; a break fails them silently.
- **Destructive order**: a pipeline step that can change a VM before preflight ends is a blocking
  finding.

## Parity with `vm`

Applies until the setup repo deletes `scripts/vm/` (roadmap phase 2,
[PRODUCT.md](PRODUCT.md#roadmap)). The reference is `scripts/vm/vm.sh` in the setup repo
(https://github.com/carlo9890/setup/tree/main/scripts/vm).

- A change to `list`, `start`, `stop`, `snapshot`, `snapshots`, `reset`, `prepare` or `provision`
  keeps the behavior of the matching `cmd_<command>` function in `vm.sh` and of the helpers it
  calls. Compare function by function: arguments, refusals, questions, order of steps, timings.
- A deliberate difference from `vm` is written in [DESIGN.md](DESIGN.md) in the same PR. Flag a
  difference that DESIGN.md does not state.

## Agent contract

The contract is in [PRODUCT.md](PRODUCT.md#agent-experience) and
[DESIGN.md](DESIGN.md#agent-interface); the mechanisms are in
[CODING.md, "Where code goes"](CODING.md#where-code-goes). Check each command the diff adds or
changes:

- **JSON**: flag a write to stdout under `--json` other than the one result document.
- **Exit codes**: flag a changed code for an existing kind, or a new kind without its entry.
- **No prompt without a TTY**: flag a question that reads stdin directly instead of using the
  confirmation mechanism.
- **Secrets**: flag a secret in argv of any process, in a log line, in an error message, or in
  JSON.

## Pipeline

The step model is in [DESIGN.md](DESIGN.md#step-model); the step rules are in
[CODING.md, "Add a provision step"](CODING.md#add-a-provision-step). Check each step the diff adds
or changes in `internal/steps/`:

- **Preflight**: flag a validation, question or secret declaration in `Run` that can fail after an
  earlier step changed a VM.
- **Cleanup**: flag a cleanup that fails when run twice, after a half-done `Run`, or when the thing
  it removes is already gone.

## Tests

The test layers are in [TESTING.md](TESTING.md).

- **Policy paths**: each refusal, guard and error kind the diff adds has a unit test on the fake
  hypervisor `virttest.New()`; `Fake.FailOn` injects a libvirt error, `Fake.Calls` proves that no
  change happened. Flag a policy path without one.
- **libvirt behavior**: a change that relies on how libvirt or QEMU behaves has a live test with a
  control step that shows the effect is real, as in
  `internal/virt/libvirt/uefi_reset_live_test.go`.

## Host safety

- Flag a test that names any domain without `livetest.DomainPrefix` for anything but a read.
- Flag code or a test that breaks [CODING.md, "Act only on test domains"](CODING.md#act-only-on-test-domains).

## Docs in the same PR

- A change to a package, a mechanic, an exit code or a step keeps [DESIGN.md](DESIGN.md) true in
  the same PR. Flag a statement in DESIGN.md that the diff makes false.

## Out of scope / non-blocking

- Formatting and the checks of `.golangci.yml` belong to `mise run quality`; do not flag what it
  reports.
