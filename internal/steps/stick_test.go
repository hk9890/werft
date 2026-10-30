package steps_test

import (
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
)

// The key stick scenarios of vm_test.sh: the host lets go of the stick only
// for the guest, and gets it back however the run ends.

func TestProvisionNeedsTheStick(t *testing.T) {
	w := newWorld(t)
	w.host.stickErr = failure.New(failure.NotFound, "nothing is mounted at /media/tester/MASTER-KEY; plug in the stick")
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.NotFound) || !strings.Contains(err.Error(), "plug in the stick") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^(secret|pass show|RevertSnapshot|StartDomain)"); i >= 0 {
		t.Errorf("a missing stick got as far as %q", w.entries()[i])
	}
}

func TestAFailedAttachStillGivesTheStickBack(t *testing.T) {
	w := newWorld(t)
	w.fake.FailOn("AttachDevice", failure.New(failure.Failed, "no such USB device"))
	if _, err := provision(t, w, true); err == nil {
		t.Fatal("provision succeeded")
	}
	w.assertSequence("the stick goes back to the host", "^host unmount stick$", "^AttachDevice testvm$", "^host mount stick$")
	if w.index("^DetachDevice") >= 0 {
		t.Error("a stick that never reached the guest was detached")
	}
}

func TestAStickTheGuestDoesNotMountIsGivenBack(t *testing.T) {
	w := newWorld(t)
	w.shell.fail["findmnt -rn -S UUID="] = true
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "the stick did not mount in testvm") {
		t.Fatalf("err = %v", err)
	}
	w.assertSequence("the stick leaves the guest, then mounts on the host",
		"^AttachDevice testvm$", "^ssh udisksctl unmount -b /dev/disk/by-uuid/ABCD-1234$", "^DetachDevice testvm$", "^host mount stick$")
	if w.index("bootstrap.sh") >= 0 {
		t.Error("the bootstrap ran without the stick")
	}
}

// A run that stops before the hand-over leaves the stick on the host.
func TestTheStickStaysOnTheHostWithoutASession(t *testing.T) {
	w := newWorld(t)
	w.shell.fail["pgrep -u"] = true
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "no desktop session of tester came up in testvm") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^(host unmount stick|AttachDevice|host mount stick)"); i >= 0 {
		t.Errorf("the stick was touched: %q", w.entries()[i])
	}
	w.assertSequence("automatic login is still restored", "pgrep -u", "mv -f .*custom.conf")
}
