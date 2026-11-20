package steps_test

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/steps"
)

// The failure scenarios of vm_test.sh: what a failed step leaves behind,
// and what never runs after it.

// noClone fails the test when the run made a clone.
func (w *world) noClone() {
	w.t.Helper()
	if i := w.index("^(CreateVolumeFrom|DefineDomain) testvm-full"); i >= 0 {
		w.t.Errorf("the failed run made the clone: %q", w.entries()[i])
	}
}

func TestProvisionWithoutSSHAnswer(t *testing.T) {
	w := newWorld(t)
	w.shell.fail["^true$"] = true
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "no SSH answer from testvm") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^(ssh sudo|host unmount stick|AttachDevice)"); i >= 0 {
		t.Errorf("a step after wait-ssh ran: %q", w.entries()[i])
	}
}

func TestSecretServiceNeverAnswers(t *testing.T) {
	w := newWorld(t)
	w.shell.fail["Secret.Service Collections"] = true
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "the Secret Service did not answer in testvm") {
		t.Fatalf("err = %v", err)
	}
	w.assertSequence("automatic login is still restored", "Secret.Service Collections", "mv -f .*custom.conf")
	if w.index("bootstrap.sh") >= 0 {
		t.Error("the bootstrap ran")
	}
}

func TestKeyringFailures(t *testing.T) {
	tests := []struct{ fail, want string }{
		{"unlock-keyring.js", "the password did not unlock or create the default keyring in testvm"},
		{"grep -qx 'b false'", "the default keyring in testvm is still locked"},
	}
	for _, tt := range tests {
		w := newWorld(t)
		w.shell.fail[tt.fail] = true
		_, err := provision(t, w, true)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v", tt.fail, err)
			continue
		}
		if w.index("bootstrap.sh") >= 0 || w.index("^AttachDevice") >= 0 {
			t.Errorf("%s: the run went on to the stick or the bootstrap", tt.fail)
		}
		w.assertSequence(tt.fail+": automatic login is restored", "mv -f .*custom.conf")
	}
}

// A file the guest half wrote is still put back.
func TestAutologinThatFailsIsStillRestored(t *testing.T) {
	w := newWorld(t)
	w.shell.fail["AutomaticLoginEnable=true"] = true
	_, err := provision(t, w, true)
	if err == nil || !strings.Contains(err.Error(), "could not turn on automatic login in testvm") {
		t.Fatalf("err = %v", err)
	}
	w.assertSequence("the guest's own file goes back", "AutomaticLoginEnable=true", "mv -f .*custom.conf")
}

func TestFailedRevokeOrRestoreMakesNoClone(t *testing.T) {
	for _, pattern := range []string{"rm -f .* sh '/etc/sudoers.d/90-werft-provision'$", "mv -f .*custom.conf"} {
		w := newWorld(t)
		w.shell.fail[pattern] = true
		if _, err := provision(t, w, true); err == nil {
			t.Errorf("%s: provision succeeded", pattern)
			continue
		}
		w.noClone()
	}
}

func TestProvisionWithoutDisplay(t *testing.T) {
	w := newWorld(t)
	w.host.display = false
	if _, err := provision(t, w, true); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if w.index("^viewer open") >= 0 {
		t.Error("a window opened without a display")
	}
	if !strings.Contains(w.out.String(), "virt-viewer -c qemu:///system --wait --reconnect testvm") {
		t.Errorf("no hint to open a window:\n%s", w.out.String())
	}
}

func TestFailedViewerSettingsCopyIsNoFailure(t *testing.T) {
	w := newWorld(t)
	w.host.settingsErr = errors.New("dconf: no session bus")
	if _, err := provision(t, w, true); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !strings.Contains(w.out.String(), "Could not copy virt-manager's viewer settings to testvm-full") {
		t.Errorf("no warning:\n%s", w.out.String())
	}
}

func TestConsoleThatNeverSettlesGetsNoKeys(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	var black bytes.Buffer
	must(t, png.Encode(&black, image.NewRGBA(image.Rect(0, 0, 4, 4))))
	w.fake.SetScreenshot(vm, "image/png", black.Bytes())
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "virt-viewer -c qemu:///system --wait --reconnect testvm") {
		t.Fatalf("err = %v", err)
	}
	if w.index("^SendKey") >= 0 {
		t.Error("keys were typed at a console with no prompt")
	}
}

// cryptsetup allows few tries, so a disk that stays locked gets the password
// once and a hint.
func TestDiskThatStaysLockedIsTypedOnce(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	w.shell.fail["^true$"] = true
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "the disk refused it") {
		t.Fatalf("err = %v", err)
	}
	if n := len(w.fake.SentKeys(vm)); n != len(password)+1 {
		t.Errorf("%d keys typed, want the password and Enter once", n)
	}
}

func TestFailedOmarchyUpdate(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	w.shell.fail["omarchy update"] = true
	if _, err := provision(t, w, true); err == nil {
		t.Fatal("provision succeeded")
	}
	w.assertSequence("sudo is closed again", "omarchy update", "rm -f .* sh '/etc/sudoers.d/90-werft-provision'$")
	if i := w.index("^(host unmount stick|AttachDevice)"); i >= 0 {
		t.Errorf("the stick was touched: %q", w.entries()[i])
	}
}

// A domain that runs already can show its desktop: the disk password would
// go to the window that has focus.
func TestBootOfARunningDomainTypesNothing(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	must(t, w.fake.StartDomain(vm))
	if _, err := w.run([]pipeline.Entry{{Name: "boot", Step: steps.Boot{}}}, true); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if w.index("^SendKey") >= 0 || w.index("^StartDomain") >= 0 {
		t.Errorf("want no start and no keys:\n  %s", strings.Join(w.entries(), "\n  "))
	}
	if !strings.Contains(w.out.String(), "testvm runs already, so werft types no disk password") {
		t.Errorf("no hint:\n%s", w.out.String())
	}
}

// After a restart of libvirtd every call fails; a step must not take that
// for a stopped domain and start it.
func TestBootStepsFailWhenTheStateIsUnknown(t *testing.T) {
	for name, step := range map[string]pipeline.Step{"boot": steps.Boot{}, "prepare boot": &steps.PrepareBoot{}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.fake.FailOn("DomainState", errors.New("lost the connection to libvirt"))
			_, err := w.run([]pipeline.Entry{{Name: "boot", Step: step}}, true)
			if err == nil || !strings.Contains(err.Error(), "could not read the state of testvm: lost the connection to libvirt") {
				t.Errorf("err = %v, want the state error", err)
			}
			if w.index("^StartDomain") >= 0 {
				t.Errorf("the step started the domain:\n  %s", strings.Join(w.entries(), "\n  "))
			}
		})
	}
}

// A state read that fails says nothing about the power of the domain:
// poweroff and reset stop at once with that error, and nothing forces the
// domain off.
func TestPowerStepsFailWhenTheStateIsUnknown(t *testing.T) {
	for name, step := range map[string]pipeline.Step{"poweroff": steps.Poweroff{}, "reset": steps.Reset{}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			must(t, w.fake.StartDomain(vm))
			r := w.newRun(true)
			sleeps := 0
			r.Sleep = func(time.Duration) { sleeps++ }
			w.fake.FailOn("DomainState", errors.New("lost the connection to libvirt"))
			err := step.Run(r)
			if err == nil || !strings.Contains(err.Error(), "could not read the state of testvm: lost the connection to libvirt") {
				t.Errorf("err = %v, want the state error", err)
			}
			if sleeps > 0 {
				t.Errorf("the step waited %d times for a state it cannot read", sleeps)
			}
			if w.index("^(DestroyDomain|RevertSnapshot)") >= 0 {
				t.Errorf("the step forced the domain off or reset it:\n  %s", strings.Join(w.entries(), "\n  "))
			}
		})
	}
}

// A disk prompt that never shows because libvirt is gone gets the libvirt
// error, not the hint of a disk opened by hand.
func TestPromptTimeoutNamesALostConnection(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	r := w.newRun(true)
	r.Sleep = func(time.Duration) {
		w.fake.FailOn("Screenshot", errors.New("lost the connection to libvirt"))
		w.fake.FailOn("DomainState", errors.New("lost the connection to libvirt"))
	}
	err := pipeline.Execute(r, bootOnly())
	if err == nil || !strings.Contains(err.Error(), "waiting for the disk prompt: could not read the state of testvm: lost the connection to libvirt") {
		t.Errorf("err = %v, want the state error", err)
	}
	if err != nil && strings.Contains(err.Error(), "opened by hand") {
		t.Errorf("err = %v, want no viewer hint", err)
	}
}

func TestCleanupSaysWhenTheStateIsUnknown(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.StartDomain(vm))
	r := w.newRun(true)
	r.Reach("true")
	g := &steps.SudoGrant{}
	must(t, g.Run(r))
	w.fake.FailOn("DomainState", errors.New("lost the connection to libvirt"))
	err := g.Cleanup(r)
	if err == nil || !strings.Contains(err.Error(), "could not read the state of testvm: lost the connection to libvirt") || strings.Contains(err.Error(), "is not running") {
		t.Errorf("err = %v, want the state error", err)
	}
	w.fake.FailOn("DomainState", nil)
	must(t, w.fake.DestroyDomain(vm))
	if err := g.Cleanup(r); err == nil || !strings.Contains(err.Error(), "testvm is not running") {
		t.Errorf("err of a stopped domain = %v, want is not running", err)
	}
}
