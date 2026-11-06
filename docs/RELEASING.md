# Releasing

`.github/workflows/release.yml` publishes a release with GoReleaser (`.goreleaser.yaml`). You
dispatch it by hand against a release tag; a tag push alone publishes nothing, and a dispatch on a
branch fails. A read-only job passes `mise run quality` on the tagged commit; only then does a
second job, the one with write access, run GoReleaser.

A release holds one archive per architecture, `werft_X.Y.Z_linux_x64.tar.gz` and
`werft_X.Y.Z_linux_arm64.tar.gz`, and `werft_X.Y.Z_checksums.txt`. mise's `github` backend picks
the archive by these names. The release notes are GoReleaser's commit list since the last tag.

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
2. From the main checkout, create a worktree detached at `origin/main` under `/tmp`, and run the
   gate in it. A hook on this host blocks `git worktree add` under `.claude/worktrees/`.

   ```bash
   WT=/tmp/werft-release-vX.Y.Z
   git fetch origin
   git worktree add --detach "$WT" origin/main
   mise -C "$WT" run quality
   ```

3. From the main checkout, check that the worktree is at `origin/main`, tag its commit, push the
   tag, then remove the worktree. Each code block sets `WT` itself, so a fresh shell runs it; the
   chain stops when the worktree is not at `origin/main`.

   ```bash
   WT=/tmp/werft-release-vX.Y.Z
   test "$(git -C "$WT" rev-parse HEAD)" = "$(git rev-parse origin/main)" &&
     git -C "$WT" tag -a vX.Y.Z -m "werft vX.Y.Z" &&
     git push origin vX.Y.Z &&
     git worktree remove "$WT"
   ```

4. Dispatch the workflow on the tag and watch it. The run takes a few seconds to register; re-run
   the lookup if it returns nothing.

   ```bash
   gh workflow run release.yml --ref vX.Y.Z
   gh run watch "$(gh run list --workflow=release.yml --branch vX.Y.Z --limit 1 --json databaseId -q '.[0].databaseId')" --exit-status
   ```

5. Verify. Expect the two archives and the checksums file.

   ```bash
   gh release view vX.Y.Z --json assets -q '.assets[].name'
   mise exec github:hk9890/werft@X.Y.Z -- werft version    # prints werft vX.Y.Z
   ```

**Dispatch on the tag, not on `main`.** `workflow_dispatch` reads the workflow from the ref it
runs on, and GoReleaser needs the checked-out commit to be the tagged one. To fix the release
workflow for a tag: land the fix by PR, then repeat step 2 on the new `origin/main`. Repeat
step 3 with `tag -f -a` and `git push -f origin vX.Y.Z`, so the tag moves inside `"$WT"` and the
worktree is removed. Then repeat steps 4 and 5. A second dispatch on the same tag replaces its
assets (`release.replace_existing_artifacts`).

## Install through mise

mise reads a private repository's releases with the first GitHub token it finds:
`MISE_GITHUB_TOKEN`, `GITHUB_API_TOKEN`, `GITHUB_TOKEN`, then the gh CLI login. The token needs
the `repo` scope, or Contents: read for a fine-grained token. `mise token github` shows which token
mise uses.

```bash
mise use -g github:hk9890/werft@X.Y.Z
```

On this host `~/.config/mise` is a stow link into the setup repo, so `mise use -g` writes into
`~/setup/dotfiles/mise/.config/mise/config.toml`. The setup repo keeps that werft line; commit it
there.

An exact version passes mise's `minimum_release_age`. `latest` skips a release younger than that
age until `github:hk9890/werft` is in `minimum_release_age_excludes`.
