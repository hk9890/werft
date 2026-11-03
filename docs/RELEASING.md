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
2. From the main checkout's root, create a worktree detached at `origin/main` and run the gate in
   it:

   ```bash
   git fetch origin
   git worktree add --detach .claude/worktrees/release-vX.Y.Z origin/main
   mise -C .claude/worktrees/release-vX.Y.Z run quality
   ```

3. Tag the worktree's commit, push the tag, then remove the worktree:

   ```bash
   git -C .claude/worktrees/release-vX.Y.Z tag -a vX.Y.Z -m "werft vX.Y.Z"
   git push origin vX.Y.Z
   git worktree remove .claude/worktrees/release-vX.Y.Z
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
workflow for a tag: land the fix by PR, repeat step 2 on the new `origin/main`, move the tag with
`tag -f` and `git push -f origin vX.Y.Z`, then repeat steps 4 and 5. A second dispatch on the
same tag replaces its assets (`release.replace_existing_artifacts`).

## Install through mise

mise reads a private repository's releases with the first GitHub token it finds:
`MISE_GITHUB_TOKEN`, `GITHUB_API_TOKEN`, `GITHUB_TOKEN`, then the gh CLI login. The token needs
the `repo` scope, or Contents: read for a fine-grained token. `mise token github` shows which token
mise uses.

```bash
mise use -g github:hk9890/werft@X.Y.Z
```

An exact version passes mise's `minimum_release_age`. `latest` skips a release younger than that
age until `github:hk9890/werft` is in `minimum_release_age_excludes`.
