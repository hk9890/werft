# Change Workflow

## Branches

Develop every change on a branch. `main` changes only by a merged PR.

- Create a worktree with Claude Code's `EnterWorktree` tool; it lands under `.claude/worktrees/`,
  which `.gitignore` covers. Use `git switch -c <type>/<topic> main` only when no other agent can
  use this checkout.
- Use the commit type as the branch prefix: `feat/resetpoints`, `fix/parity-gaps`.
- Dependabot PRs keep their `dependabot/` branches; merge them as "Pull requests" says.

## Issues

Call the Skill tool with `tasks:tasks-core` before you touch an issue.

**Local delta:**

- The store is the central store `werft`; `taskmgr where` resolves it from this checkout.
- Run `taskmgr update <id> --status in_progress` before the first implementation edit.
- Run `taskmgr close <id> --reason "<PR URL>"` after the PR merges.

## Commits

Call the Skill tool with `commit-commands:commit` for the standard flow.

**Local delta:** write Conventional Commits with an optional scope: `feat: add werft list, start
and stop`, `fix(provision): name the stick at its mount in the hand-over message`.

## Before push

Pass the checks in [TESTING.md](TESTING.md#minimum-checks-before-opening-a-pr); when the live
checks ran, say so in the PR body.

## Pull requests

Call the Skill tool with `commit-commands:commit-push-pr` for the standard flow.

**Local delta:**

1. Open the PR against `main` with `gh pr create`.
2. Run `gh pr checks --watch` and fix every red check before the merge.
3. Merge with `gh pr merge --merge`. The history keeps merge commits; use no squash and no rebase.
   Do not add `--delete-branch` from a worktree: it switches to `main`, which the main checkout
   holds.
4. Leave and remove the worktree with `ExitWorktree`.
5. Run `git pull --ff-only` in the main checkout, then `git push origin --delete <branch>`.

## Session close

Before you treat a session as finished:

- Every PR of the session is merged, or handed to the user with its URL.
- No worktree of the session is left under `.claude/worktrees/`: `git worktree list`.
- Every issue of the session has its status: `closed` with the PR URL, or `open` with a note on
  what is left.
