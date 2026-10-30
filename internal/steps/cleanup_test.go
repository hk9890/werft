package steps_test

import (
	"testing"

	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/steps"
)

// Every cleanup is safe to run twice: the second run does nothing.
func TestCleanupsRunTwice(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.StartDomain(vm))
	r := w.newRun(true)
	r.Family = "ubuntu"
	cleaners := []interface {
		pipeline.Step
		pipeline.Cleaner
	}{&steps.Autologin{}, &steps.SudoGrant{}, &steps.Pinentry{Secret: "passphrase"}, &steps.UsbPassthrough{Label: "MASTER-KEY"}, steps.Viewer{}}
	if !r.Reach("true") {
		t.Fatal("the guest does not answer")
	}
	for _, c := range cleaners {
		must(t, c.Check(r))
		must(t, c.Run(r))
	}
	before := len(w.entries())
	for _, c := range cleaners {
		must(t, c.Cleanup(r))
	}
	first := len(w.entries()) - before
	for _, c := range cleaners {
		must(t, c.Cleanup(r))
	}
	if again := len(w.entries()) - before - first; again != 0 {
		t.Errorf("the second cleanup did %d things: %q", again, w.entries()[before+first:])
	}
	if first == 0 {
		t.Error("the first cleanup did nothing")
	}
}

// A step that failed half-way is still undone: the cleanup acts on what the
// step may have written.
func TestCleanupAfterAFailedGrant(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.StartDomain(vm))
	w.shell.fail["NOPASSWD"] = true
	r := w.newRun(true)
	r.Reach("true")
	g := &steps.SudoGrant{}
	if err := g.Run(r); err == nil {
		t.Fatal("the grant succeeded")
	}
	must(t, g.Cleanup(r))
	w.assertSequence("the failed grant is taken back", "NOPASSWD", "rm -f .*90-werft-provision")
}

// A cleanup of the build VM still reaches it after the run moved on to the
// clone.
func TestCleanupActsOnItsOwnDomain(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.StartDomain(vm))
	r := w.newRun(true)
	r.Reach("true")
	g := &steps.SudoGrant{}
	must(t, g.Run(r))
	r.Domain = "testvm-full"
	must(t, g.Cleanup(r))
	if r.Domain != "testvm-full" {
		t.Errorf("the cleanup left the run on %s", r.Domain)
	}
	w.assertSequence("the grant is taken back", "NOPASSWD", "rm -f .*90-werft-provision")
}
