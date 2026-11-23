# Testing

## Test layers

| Layer | Location | Command | Runs in CI |
|-------|----------|---------|------------|
| Unit | `*_test.go` next to source | `mise run test` | yes |
| Live | `*_live_test.go`, build tag `live` | `mise run test:live` | no: GitHub runners have no KVM |

Both layers run with the race detector, which needs a C compiler (`gcc`) on the host.

A live test drives the real `qemu:///system`. It skips when the libvirt socket does not answer or
the test image is missing.

## Live test setup

1. Put your user in group `libvirt`. Install `qemu-img` and `e2fsprogs`: `TestSealLive` reads the
   stock disk with `debugfs`.
2. Download the test image once, and check it against `SHA256SUMS` from the same directory:

   ```sh
   mkdir -p ~/.cache/werft/images && cd ~/.cache/werft/images
   curl -fLO https://cloud-images.ubuntu.com/minimal/releases/resolute/release/ubuntu-26.04-minimal-cloudimg-amd64.img
   curl -fLO https://cloud-images.ubuntu.com/minimal/releases/resolute/release/SHA256SUMS
   sha256sum -c --ignore-missing SHA256SUMS
   ```

   Set `WERFT_TEST_IMAGE` to use another UEFI cloud image.
3. For a test on systemd 255, download `ubuntu-24.04-minimal-cloudimg-amd64.img` the same way from
   `https://cloud-images.ubuntu.com/minimal/releases/noble/release/`. Set `WERFT_TEST_IMAGE_255`
   to use another image. `livetest.NewTCPGuest` skips the test when the image is missing.
4. Run `mise run test:live`.

What a run creates:

- The storage pool `werft-test` in `/var/lib/libvirt/images/werft-test/`. It stays, with the uploaded
  base image, so the next run skips the upload. The base volume has the name of the image file.
  `~/.cache/werft/base-image.lock` lets one run at a time upload it, and a marker file next to the
  lock records a finished upload; without the marker, the next run uploads again.
- Each test makes domains and volumes named `werft-test-<track>-*` in pool `werft-test` and removes
  them. List them with the `werft-test-` expression in [OVERVIEW.md](OVERVIEW.md#finding-things).

To remove the pool:

1. List its volumes: `virsh -c qemu:///system vol-list werft-test`.
2. Delete each volume: `virsh -c qemu:///system vol-delete --pool werft-test <vol>`.
3. Stop the pool: `virsh -c qemu:///system pool-destroy werft-test`.
4. Remove it: `virsh -c qemu:///system pool-undefine werft-test`.

No test changes a build VM. `TestLifecycleLive` runs `werft list`, which reads every domain on the
host, build VMs included, and changes none.

## Debug a failing live test

- A boot that times out prints the last 60 lines of the guest serial console.
- `WERFT_TEST_KEEP=1` keeps the domain after a failure. Connect with
  `ssh -F /dev/null -o "ProxyCommand=/usr/lib/systemd/systemd-ssh-proxy %h %p" -o ProxyUseFdpass=yes root@vsock/<cid>`;
  the CID is in `virsh -c qemu:///system dumpxml <domain>`. The test key is gone with the
  test's temp directory, so only the connection itself can be checked. The next run removes the
  domain.

## Write a live test

Build on `internal/livetest`; `internal/virt/libvirt/uefi_reset_live_test.go` is the model:

- Create the scratch domain with `livetest.NewGuest`, and remove other domains with
  `livetest.RemoveDomain`. Both refuse a name without the `werft-test-` prefix. `NewGuest`
  removes leftovers of an earlier run, and removes the domain and its volumes in `t.Cleanup`.
- Give each domain and volume the prefix of your track, so parallel runs do not collide.
- Before a test runs `werft lease`, set `leasePrefix` (`cmd/werft/lease.go`) to the prefix of
  its track, as `TestLeaseLive` does; the default `werft-` names no test domain.
- To make a lease expire, move `clock` (`cmd/werft/lease.go`) forward with `setClock`, as
  `leaseLifetime` in `cmd/werft/lease_live_test.go` does.
- Reach the guest with `Guest.Boot`, `Guest.Run` and `Guest.SSH`: root over vsock, with the test key
  as the `ssh.ephemeral-authorized_keys-all` credential. The cloud image has no SSH host keys and
  no network configuration, so the guest gets two more credentials: a `sshd-vsock@` drop-in that
  runs `ssh-keygen -A`, and a networkd DHCP file.
- For a systemd 255 guest, create the domain with `livetest.NewTCPGuest`. It has no vsock device.
  `Guest.Boot` and `Guest.SSH` use root at the DHCP address, with the key, the networkd file and
  the host-key units of `seal.sh` in `tmpfiles.extra`. An OEM string is in the argv of QEMU, so it
  holds no private key. `Guest.Boot` boots twice: the guest reads these files only at the second
  boot.
- Call `Guest.Run` and `Guest.Boot` only from the test goroutine, not from a subtest: they fail
  the test they were made for.
- Add a control step that shows the effect you assert away is real.

## Minimum checks before opening a PR

1. `mise run quality` passes. CI runs the same task, then `mise run vuln`.
2. If a change touches `internal/virt/` or anything a live test covers: `mise run quality:full`
   passes.
