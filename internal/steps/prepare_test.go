package steps_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

func freshWorld(t *testing.T) *world {
	w := newWorld(t)
	must(t, w.fake.DeleteSnapshot(vm, steps.ProvisionBase))
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
		"^ssh-copy-id .*/id_ed25519.pub tester@192.168.122.01$", "^ssh true$", "sh -c 'true' sh$")
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
	added := slices.ContainsFunc(w.shell.stdin, func(s string) bool {
		cmd, data, _ := strings.Cut(s, "\x00")
		return strings.Contains(cmd, "authorized_keys") && data == werftPubKey+"\n"
	})
	if !added {
		t.Errorf("authorized_keys did not get the werft key: %q", w.shell.stdin)
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
	if desc != "my notes\n"+steps.LUKSMarker {
		t.Errorf("description = %q", desc)
	}
	if w.index("^SendKey") >= 0 {
		t.Error("prepare typed the disk password")
	}
}

func TestPrepareClearsAStaleMarker(t *testing.T) {
	w := freshWorld(t)
	must(t, w.fake.SetDomainDescription(vm, "my notes\n"+steps.LUKSMarker))
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
