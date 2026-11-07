# Releasing

`.github/workflows/release.yml` publishes a release with GoReleaser (`.goreleaser.yaml`). You
dispatch it by hand against a release tag; a tag push alone publishes nothing, and a dispatch on a
branch fails. A read-only job passes `mise run quality` on the tagged commit; only then does a
second job, the one with write access, run GoReleaser.

A release holds one archive per architecture, `werft_X.Y.Z_linux_x64.tar.gz` and
`werft_X.Y.Z_linux_arm64.tar.gz`, and `werft_X.Y.Z_checksums.txt`. mise's `github` backend picks
the archive by these names. The release notes are the tag's `CHANGELOG.md` section, printed by
`scripts/release-notes vX.Y.Z`; a tag with no section fails the run before it publishes.

The release has no cosign signature, no SBOM and no build-provenance attestation. The repository
is private and werft has one user; add them when werft goes public.

## Check the config

Run both after a change to `.goreleaser.yaml`. `mise run quality` runs `release:check` too.

```bash
mise run release:check       # goreleaser check
mise run release:snapshot    # archives into dist/, publishes nothing
./dist/werft_linux_amd64_v1/werft version
```

## Cut a release

1. Pick `vX.Y.Z` and confirm it is free: `git fetch --tags origin && git tag --list "v*"`.
2. Write the `## vX.Y.Z` section of `CHANGELOG.md`, as
   [What goes in the CHANGELOG section](#what-goes-in-the-changelog-section) says, and land it like
   any other change: worktree, PR, merge ([CHANGE-WORKFLOW.md](CHANGE-WORKFLOW.md)).
3. From the main checkout, create a worktree detached at `origin/main` under `/tmp`, and run the
   gate in it. A hook on this host blocks `git worktree add` under `.claude/worktrees/`. The
   script must find the section, and the log lists every commit since the previous release.
   Check that each change a user can see has an entry.

   ```bash
   WT=/tmp/werft-release-vX.Y.Z
   git fetch --tags origin
   git worktree add --detach "$WT" origin/main
   mise -C "$WT" run quality
   "$WT/scripts/release-notes" vX.Y.Z
   prev=$(git -C "$WT" describe --tags --abbrev=0 --exclude vX.Y.Z) &&
     git -C "$WT" log --no-merges --oneline "$prev"..HEAD
   ```

   A missing entry or section goes back to step 2. Remove the worktree first, so step 3 can make it
   again: `git worktree remove /tmp/werft-release-vX.Y.Z`.

4. From the main checkout, check that the worktree is at `origin/main`, tag its commit, push the
   tag, then remove the worktree. Each code block sets `WT` itself, so a fresh shell runs it; the
   chain stops when the worktree is not at `origin/main`.

   ```bash
   WT=/tmp/werft-release-vX.Y.Z
   test "$(git -C "$WT" rev-parse HEAD)" = "$(git rev-parse origin/main)" &&
     git -C "$WT" tag -a vX.Y.Z -m "werft vX.Y.Z" &&
     git push origin vX.Y.Z &&
     git worktree remove "$WT"
   ```

5. Dispatch the workflow on the tag and watch it. The run takes a few seconds to register; re-run
   the lookup if it returns nothing.

   ```bash
   gh workflow run release.yml --ref vX.Y.Z
   gh run watch "$(gh run list --workflow=release.yml --branch vX.Y.Z --limit 1 --json databaseId -q '.[0].databaseId')" --exit-status
   ```

6. Verify from the main checkout, after `git pull --ff-only`. Expect the two archives and the
   checksums file, a binary that prints `werft vX.Y.Z`, and a body that is the tag's
   `CHANGELOG.md` section: the `diff` prints nothing when they match.

   ```bash
   gh release view vX.Y.Z --json assets -q '.assets[].name'
   D=$(mktemp -d) && gh release download vX.Y.Z -p '*_linux_x64.tar.gz' -D "$D" &&
     tar -xzf "$D"/*.tar.gz -C "$D" && "$D/werft" version
   diff <(gh release view vX.Y.Z --json body -q .body) <(scripts/release-notes vX.Y.Z; echo)
   ```

   When the body differs, set it from the section. This needs no new dispatch:

   ```bash
   scripts/release-notes vX.Y.Z > /tmp/werft-notes-vX.Y.Z.md &&
     gh release edit vX.Y.Z --notes-file /tmp/werft-notes-vX.Y.Z.md
   ```

**Dispatch on the tag, not on `main`.** `workflow_dispatch` reads the workflow from the ref it
runs on, and GoReleaser needs the checked-out commit to be the tagged one. To fix the release
workflow for a tag: land the fix by PR, then repeat step 3 on the new `origin/main`. Repeat
step 4 with `tag -f -a` and `git push -f origin vX.Y.Z`, so the tag moves inside `"$WT"` and the
worktree is removed. Then repeat steps 5 and 6. A second dispatch on the same tag replaces its
assets (`release.replace_existing_artifacts`) but keeps its body.

## What goes in the CHANGELOG section

The section is the release text on GitHub. Write it for whoever runs `werft` and has never opened
the source. List features, changed behavior and fixes, never a commit list. An entry earns its
place by telling the reader what they must do, what they will see that they did not see before, or
what was wrong that is now fixed. State the symptom; the cause belongs in the commit message.

- Open each entry with a bold sentence that says what is true now. For changed behavior or a fix,
  then say what it was before. A new feature needs no "before".
- Never name an internal symbol, a Go package, a third-party library, or a test.
- A change with no visible effect gets no entry: a refactor, added tests, a doc fix, a dependency
  bump nobody has to act on, a fix of the release workflow.
- Every other change a user can see since the last tag gets an entry, each fix included. Several
  commits of one change share one entry. A fix of a defect that no tagged release had gets none.
- Anything that now refuses a config, a flag or a habit the user already has leads with
  **Action required** and names the fix.
