package steps_test

import (
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/steps"
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

// A stick the guest keeps is not on the host: the cleanup does not say it
// is, and the end of the run tries the detach again.
func TestAFailedDetachIsTriedAgain(t *testing.T) {
	w := newWorld(t)
	w.fake.FailOn("DetachDevice", failure.New(failure.Failed, "device busy"))
	if _, err := provision(t, w, true); err == nil {
		t.Fatal("provision succeeded")
	}
	if n := w.count("^DetachDevice testvm$"); n != 2 {
		t.Errorf("detach ran %d times, want in undo and again at the end", n)
	}
	if w.index("^host mount stick$") >= 0 || strings.Contains(w.out.String(), "The stick is back on the host") {
		t.Errorf("the stick counts as back on the host:\n%s", w.out.String())
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

// --key points provision at a stick mounted elsewhere than its label says.
func TestProvisionTakesTheStickAtKeyMount(t *testing.T) {
	w := newWorld(t)
	r := w.newRun(true)
	r.KeyMount = "/media/tester/BACKUP"
	if err := pipeline.Execute(r, steps.FromRecipe(setupRecipe(t))); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if w.index("^find stick /media/tester/BACKUP$") < 0 || w.index("^find stick /media/tester/MASTER-KEY$") >= 0 {
		t.Errorf("want the stick at --key only:\n  %s", strings.Join(w.entries(), "\n  "))
	}
	if out := w.out.String(); !strings.Contains(out, "Handing the stick at /media/tester/BACKUP to testvm") {
		t.Errorf("the hand-over does not name the stick at --key:\n%s", out)
	}
}
