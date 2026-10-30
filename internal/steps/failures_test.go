package steps_test

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
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
	if !strings.Contains(w.out.String(), "werft start --view testvm") {
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
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "werft start --view testvm") {
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
