//go:build live

package libvirt_test

import (
	"fmt"
	"testing"

	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/virt"
)

// The reset-point design (docs/DESIGN.md, "Reset points") rests on one claim:
// a libvirt internal snapshot of a shut-off UEFI domain whose NVRAM is qcow2
// restores the firmware variables together with the disk. This test proves it
// on a scratch domain, with the same overlay, credential and vsock mechanics a
// lease uses.

const (
	// NV | BS | RT, then the value "werft".
	efiVarValue = `\x07\x00\x00\x00werft`
	efiVarPath  = "/sys/firmware/efi/efivars/WerftResetTest-3f1b2c4d-5e6f-4a1b-9c8d-7e6f5a4b3c2d"
	diskMarker  = "/root/werft-reset-marker"
)

func TestUEFIResetPointRestoresNVRAMAndDisk(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-fnd-uefi")

	g.Boot()
	g.Poweroff()
	livetest.Must(t, h.CreateSnapshot(g.Name, "base", ""), "create reset point")
	assertSnapshotCoversNVRAM(t, h, g.Name)

	g.Boot()
	g.Run(fmt.Sprintf("printf '%s' > %s && echo changed > %s && sync", efiVarValue, efiVarPath, diskMarker))
	g.Poweroff()

	// Control: without a revert both changes survive a reboot, so their absence
	// after the revert means something.
	g.Boot()
	g.Run(fmt.Sprintf("test -e %s && test -e %s", efiVarPath, diskMarker))
	g.Poweroff()

	livetest.Must(t, h.RevertSnapshot(g.Name, "base"), "revert to reset point")

	g.Boot()
	g.Run(fmt.Sprintf("test ! -e %s", efiVarPath))
	g.Run(fmt.Sprintf("test ! -e %s", diskMarker))
	g.Poweroff()
}

// assertSnapshotCoversNVRAM guards against libvirt's raw-NVRAM trap: a snapshot
// that silently leaves the firmware state out.
func assertSnapshotCoversNVRAM(t *testing.T, h virt.Hypervisor, domain string) {
	t.Helper()
	desc, err := h.SnapshotXML(domain, "base")
	livetest.Must(t, err, "snapshot XML")
	var s libvirtxml.DomainSnapshot
	livetest.Must(t, s.Unmarshal(desc), "parse snapshot XML")
	if s.Domain == nil || s.Domain.OS == nil || s.Domain.OS.NVRam == nil || s.Domain.OS.NVRam.Format != "qcow2" {
		t.Fatalf("snapshot does not record a qcow2 NVRAM:\n%s", desc)
	}
}
