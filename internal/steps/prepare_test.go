package steps_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

func freshWorld(t *testing.T) *world {
	w := newWorld(t)
	must(t, w.fake.DeleteSnapshot(vm, resetpoint.ProvisionBase))
	return w
}

func TestPrepare(t *testing.T) {
	w := freshWorld(t)
	if _, err := w.run(steps.Prepare(), false); err != nil {
		t.Fatalf("prepare: %v\n%s", err, w.out.String())
	}
	w.assertSequence("the guest is checked, named, powered off from inside, then snapshotted",
		"^secret Password of tester", "^StartDomain testvm$", "^ssh true$", "^ssh \\[ -d /usr/share/omarchy \\]$",
		"AutomaticLoginEnable", "^ssh sudo -k -S -p '' sh -c 'true' sh$", "grep -qx crypt$",
		"hostnamectl set-hostname.* sh 'testvm'$", "systemctl poweroff", "^CreateSnapshot testvm provision-base$")
	if w.index("^ssh-copy-id") >= 0 || w.index("^viewer open") >= 0 {
		t.Error("a ready guest got the key again, or a window")
	}
	if w.index("^SetDomainDescription") >= 0 {
		t.Error("a root without LUKS changed the description")
	}
}

func TestPrepareAsksBeforeItRetakesTheBase(t *testing.T) {
	w := newWorld(t)
	w.term.tty = false
	_, err := w.run(steps.Prepare(), false)
	if !failure.Is(err, failure.NeedsConfirmation) {
		t.Fatalf("err = %v", err)
	}
	if w.index("^StartDomain") >= 0 {
		t.Error("prepare started the VM before the answer")
	}

	w = newWorld(t)
	if _, err := w.run(steps.Prepare(), true); err != nil {
		t.Fatal(err)
	}
	w.assertSequence("the old base goes before the new one is taken",
		"^DeleteSnapshot testvm provision-base$", "^CreateSnapshot testvm provision-base$")
}

func TestPrepareCopiesTheKey(t *testing.T) {
	w := freshWorld(t)
	w.shell.keyRejected = true
	if _, err := w.run(steps.Prepare(), false); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	w.assertSequence("the key goes in before the first root step",
		"^ssh-copy-id .*/werft/id_ed25519.pub tester@192.168.122.1$", "authorized_keys", "^ssh true$", "sh -c 'true' sh$")
	// sshd (PerSourcePenalties) resets a source with too many failed logins.
	if n := w.count("^ssh refused$"); n != 1 {
		t.Errorf("%d failed logins, want one before the key copy and no more", n)
	}
	if got := authorizedKeys(w); got != werftPubKey+"\n"+userPubKey+"\n" {
		t.Errorf("authorized_keys got %q, want the werft key and the user's key", got)
	}
}

// authorizedKeys is what prepare gave the guest for its authorized_keys.
func authorizedKeys(w *world) string {
	for _, s := range w.shell.stdin {
		if cmd, data, _ := strings.Cut(s, "\x00"); strings.Contains(cmd, "authorized_keys") {
			return data
		}
	}
	return ""
}

func TestPrepareNamesTheUserWhenTheKeyCopyFails(t *testing.T) {
	w := freshWorld(t)
	w.shell.keyRejected = true
	w.host.copyKeyErr = errors.New("exit status 1")
	_, err := w.run(steps.Prepare(), false)
	if err == nil || !strings.Contains(err.Error(), "the user in the VM must be tester") {
		t.Fatalf("err = %v", err)
	}
	if w.index("sh -c 'true' sh$") >= 0 {
		t.Error("prepare went on without the key")
	}
}

// A guest that accepts the werft key still gets the user's key, so ssh
// <name> works on a base werft alone prepared, and on its clone.
func TestPrepareAddsTheUsersKeyToAReadyGuest(t *testing.T) {
	w := freshWorld(t)
	if _, err := w.run(steps.Prepare(), false); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := authorizedKeys(w); got != werftPubKey+"\n"+userPubKey+"\n" {
		t.Errorf("authorized_keys got %q", got)
	}
}

func TestPrepareWithoutAUserKeyAddsTheWerftKey(t *testing.T) {
	w := freshWorld(t)
	must(t, os.Remove(w.userKey+".pub"))
	if _, err := w.run(steps.Prepare(), false); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := authorizedKeys(w); got != werftPubKey+"\n" {
		t.Errorf("authorized_keys got %q", got)
	}
}

// A guest without sshd gets the install commands and one window; with no
// SSH server in the person's time the run fails with a timeout.
func TestPrepareWithoutSSHServer(t *testing.T) {
	w := freshWorld(t)
	w.shell.fail["^true$"] = true
	_, err := w.run(steps.Prepare(), false)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "no SSH server answered in testvm") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(w.out.String(), "sudo apt install -y openssh-server") {
		t.Errorf("no install advice:\n%s", w.out.String())
	}
	if n := w.count("^viewer open testvm"); n != 1 {
		t.Errorf("%d windows opened, want one", n)
	}
	if w.index("^CreateSnapshot") >= 0 {
		t.Error("a base was taken")
	}
}

// An encrypted disk holds the guest without network until a person types
// its password; prepare says so and waits, and never types it.
func TestPrepareWithoutAddress(t *testing.T) {
	w := freshWorld(t)
	w.h.noLeases = true
	_, err := w.run(steps.Prepare(), false)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "testvm took no address") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(w.out.String(), "If its disk asks for a password, type it in its window") {
		t.Errorf("no disk password advice:\n%s", w.out.String())
	}
	if w.count("^viewer open testvm") != 1 || w.index("^SendKey") >= 0 || w.index("^ssh ") >= 0 {
		t.Errorf("want one window, no keys typed, no login:\n  %s", strings.Join(w.entries(), "\n  "))
	}
}

func TestPrepareWithoutDisplayNamesTheViewerCommand(t *testing.T) {
	w := freshWorld(t)
	w.host.display = false
	w.h.noLeases = true
	if _, err := w.run(steps.Prepare(), false); err == nil {
		t.Fatal("prepare succeeded")
	}
	if !strings.Contains(w.out.String(), "virt-viewer -c qemu:///system --wait --reconnect testvm") || w.index("^viewer open") >= 0 {
		t.Errorf("want the hint and no window:\n%s", w.out.String())
	}
}

func TestPrepareAddsTheKeyOverTheUsersOwnKey(t *testing.T) {
	w := freshWorld(t)
	w.shell.userKeyOnly = true
	if _, err := w.run(steps.Prepare(), false); err != nil {
		t.Fatalf("prepare: %v\n%s", err, w.out.String())
	}
	w.assertSequence("the werft key goes in over the working login, before the first root step",
		"^ssh true$", "^ssh umask 077 .*authorized_keys", "^ssh true$", "sh -c 'true' sh$")
	if w.index("^ssh-copy-id") >= 0 {
		t.Error("ssh-copy-id asked for a password though a key worked")
	}
	if got := authorizedKeys(w); got != werftPubKey+"\n"+userPubKey+"\n" {
		t.Errorf("authorized_keys got %q, want the werft key and the user's key", got)
	}
	if n := w.count("^ssh refused$"); n != 0 {
		t.Errorf("%d failed logins, want none: the user's key works", n)
	}
}

func TestPrepareOmarchyMarksTheDisk(t *testing.T) {
	w := freshWorld(t)
	w.shell.omarchy, w.shell.luks = true, true
	if _, err := w.run(steps.Prepare(), false); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if w.index("AutomaticLoginEnable") >= 0 {
		t.Error("an Omarchy guest was asked about GDM")
	}
	desc, _ := w.fake.DomainDescription(vm)
	if desc != "my notes\n"+domxml.LUKSMarker {
		t.Errorf("description = %q", desc)
	}
	if w.index("^SendKey") >= 0 {
		t.Error("prepare typed the disk password")
	}
}

func TestPrepareClearsAStaleMarker(t *testing.T) {
	w := freshWorld(t)
	must(t, w.fake.SetDomainDescription(vm, "my notes\n"+domxml.LUKSMarker))
	if _, err := w.run(steps.Prepare(), false); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if desc, _ := w.fake.DomainDescription(vm); desc != "my notes" {
		t.Errorf("description = %q", desc)
	}
}

func TestPrepareRefusesAutologin(t *testing.T) {
	w := freshWorld(t)
	w.shell.fail["AutomaticLoginEnable"] = true
	_, err := w.run(steps.Prepare(), false)
	if !failure.Is(err, failure.Conflict) || !strings.Contains(err.Error(), "automatic login is on in testvm") {
		t.Fatalf("err = %v", err)
	}
	if w.index("systemctl poweroff") >= 0 {
		t.Error("a guest with automatic login was powered off")
	}
	if s, _ := w.fake.DomainState(vm); s != virt.StateRunning {
		t.Errorf("testvm is %s", s)
	}
}

func TestPrepareRejectedPasswordTakesNoBase(t *testing.T) {
	w := freshWorld(t)
	w.shell.fail["sh -c 'true' sh$"] = true
	_, err := w.run(steps.Prepare(), false)
	if err == nil || !strings.Contains(err.Error(), "did not accept the password") {
		t.Fatalf("err = %v", err)
	}
	if w.index("^CreateSnapshot") >= 0 {
		t.Error("a base was taken")
	}
}

func TestPrepareInterruptedInTheSSHWaitGivesNoAdvice(t *testing.T) {
	w := freshWorld(t)
	var cancel context.CancelFunc
	w.shell.onRun = func(string) { cancel() }
	_, err := w.runWithCancel(steps.Prepare(), &cancel)
	if err == nil || !strings.Contains(err.Error(), "interrupted while waiting") {
		t.Fatalf("err = %v", err)
	}
	if w.index("^viewer open") >= 0 || strings.Contains(w.out.String(), "has no SSH server") {
		t.Errorf("an interrupt got the advice for a guest without sshd:\n%s", w.out.String())
	}
}

// A vsock CID is in the live XML from the start, while an encrypted disk
// still holds the guest without network: only a DHCP lease is an address.
func TestPrepareWaitsForALeaseNotAVsockCID(t *testing.T) {
	w := freshWorld(t)
	text, err := w.fake.DomainXML(vm, true)
	must(t, err)
	_, err = w.fake.DefineDomain(strings.Replace(text, "</devices>", "<vsock model='virtio'><cid auto='yes'/></vsock></devices>", 1))
	must(t, err)
	w.h.noLeases = true
	_, err = w.run(steps.Prepare(), false)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "testvm took no address") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(w.out.String(), "If its disk asks for a password, type it in its window") || w.count("^viewer open testvm") != 1 {
		t.Errorf("want the disk password advice and one window:\n%s", w.out.String())
	}
	if cid, _ := virt.DomainAddresses(w.fake, vm); cid == "" {
		t.Error("the domain got no vsock CID; the test proves nothing")
	}
}

// LUKSMark changes only the marker line; the user's text, blank lines too,
// stays as it is.
func TestPrepareKeepsTheBlankLinesOfTheDescription(t *testing.T) {
	const notes = "my notes\n\nmore"
	tests := []struct {
		name       string
		desc, want string
		luks       bool
	}{
		{"no LUKS", notes, notes, false},
		{"LUKS", notes, notes + "\n" + domxml.LUKSMarker, true},
		{"LUKS gone", notes + "\n" + domxml.LUKSMarker, notes, false},
		{"empty, LUKS", "", domxml.LUKSMarker, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := freshWorld(t)
			must(t, w.fake.SetDomainDescription(vm, tt.desc))
			w.shell.luks = tt.luks
			if _, err := w.run(steps.Prepare(), false); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if desc, _ := w.fake.DomainDescription(vm); desc != tt.want {
				t.Errorf("description = %q, want %q", desc, tt.want)
			}
			if tt.desc == tt.want && w.index("^SetDomainDescription") >= 0 {
				t.Error("an unchanged description was rewritten")
			}
		})
	}
}
