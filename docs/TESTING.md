# Testing

## Test layers

| Layer | Location | Command | Runs in CI |
|-------|----------|---------|------------|
| Unit | `*_test.go` next to source | `mise run test` | yes |
| Live | `*_live_test.go`, build tag `live` | `mise run test:live` | no: GitHub runners have no KVM |

A live test drives the real `qemu:///system`. It skips when the libvirt socket does not answer or
the test image is missing.

## Live test setup

1. Put your user in group `libvirt`. The tests need no sudo.
2. Download the test image once, and check it against `SHA256SUMS` from the same directory:

   ```sh
   mkdir -p ~/.cache/werft/images && cd ~/.cache/werft/images
   curl -fLO https://cloud-images.ubuntu.com/minimal/releases/resolute/release/ubuntu-26.04-minimal-cloudimg-amd64.img
   ```

   Set `WERFT_TEST_IMAGE` to use another UEFI cloud image.
3. Run `mise run test:live`.

What a run creates:

- The storage pool `werft-test` in `/var/lib/libvirt/images/werft-test/`. It stays, with the uploaded
  base image, so the next run skips the upload. Remove both with
  `virsh -c qemu:///system pool-destroy werft-test && virsh -c qemu:///system pool-undefine werft-test`
  after you delete its volumes.
- The domain `werft-test-uefi` with its overlay, NVRAM and console log. The test removes them.

The test names never touch a build VM.

## Debug a failing live test

- A boot that times out prints the last 60 lines of the guest serial console.
- `WERFT_TEST_KEEP=1` keeps the domain after a failure. Connect with
  `ssh -F /dev/null -o "ProxyCommand=/usr/lib/systemd/systemd-ssh-proxy %h %p" -o ProxyUseFdpass=yes root@vsock/<cid>`;
  the CID is in `virsh -c qemu:///system dumpxml werft-test-uefi`. The test key is gone with the
  test's temp directory, so only the connection itself can be checked. The next run removes the
  domain.

## Write a live test

Follow `internal/virt/libvirt/uefi_reset_live_test.go`:

- Use only the `werft-test` pool and domain names that start with `werft-test-`.
- Remove what the test creates in `t.Cleanup`, and remove leftovers of an earlier run at the start.
- Reach the guest over vsock with a key passed as the `ssh.ephemeral-authorized_keys-all`
  credential in the SMBIOS OEM strings. A cloud image has no SSH host keys: pass the
  `systemd.unit-dropin.sshd-vsock@.service` credential that runs `ssh-keygen -A`, as the existing
  test does.
- Add a control step that shows the effect you assert away is real.

## Minimum checks before opening a PR

1. `mise run check` passes (fmt, vet, lint and unit tests, the same as CI).
2. If a change touches `internal/virt/` or anything a live test covers: `mise run test:live`
   passes, and the PR says so.
