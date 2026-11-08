package steps_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

func provision(t *testing.T, w *world, yes bool) (*pipeline.Run, error) {
	t.Helper()
	return w.run(steps.FromRecipe(setupRecipe(t)), yes)
}

func TestProvisionUbuntu(t *testing.T) {
	w := newWorld(t)
	r, err := provision(t, w, false)
	if err != nil {
		t.Fatalf("provision: %v\n%s", err, w.out.String())
	}
	if r.Domain != "testvm-full" {
		t.Errorf("the run ends on %s, not the clone", r.Domain)
	}
	w.assertSequence("every stage runs in order, each poweroff where it belongs",
		"^RevertSnapshot testvm provision-base$", "^StartDomain testvm$", "^viewer open testvm reconnect=true$",
		"^ssh true$", "^ssh sudo -k -S -p '' sh -c 'true' sh$",
		"^ssh sudo .*hostnamectl set-hostname.* sh 'testvm'$",
		"^ssh sudo .*AutomaticLoginEnable=true.*systemctl restart gdm.* sh 'tester' '/etc/gdm3/custom.conf'$",
		"^ssh rm -rf '/tmp/werft-provision' && mkdir -p .* && tar -x",
		"^ssh pgrep -u .* 'gnome-shell'$", "Secret.Service Collections$", "gjs '/tmp/werft-provision/unlock-keyring.js'$", "grep -qx 'b false'$",
		"^host unmount stick$", "^AttachDevice testvm$", "^ssh findmnt -rn -S UUID=ABCD-1234$",
		"^ssh sudo .*NOPASSWD.* sh 'tester' '/etc/sudoers.d/90-werft-provision'$",
		"^ssh sh -c .*pinentry-program", "^ssh mkdir -p .* && bash bootstrap.sh https://github.com/carlo9890/bootstrap-scripts$",
		"^ssh sh -c .*grep -vxF", "^ssh udisksctl unmount -b /dev/disk/by-uuid/ABCD-1234$", "^DetachDevice testvm$", "^host mount stick$",
		"^ssh sudo .*rm -f .* sh '/etc/sudoers.d/90-werft-provision'$",
		"systemctl poweroff", "^StartDomain testvm$", "^ssh rm -rf '/tmp/werft-provision'", "^ssh pgrep", "unlock-keyring.js'$",
		"^ssh rm -rf '/tmp/werft-provision/system'", "^ssh cd '/tmp/werft-provision/system' && bash 'run.sh'$",
		"^viewer close testvm$", "^ssh sudo .*mv -f .* sh '/etc/gdm3/custom.conf'$", "systemctl poweroff",
		"^CreateVolumeFrom testvm-full.qcow2 testvm.qcow2$", "^CreateVolumeFrom testvm-full_VARS.qcow2 testvm_VARS.qcow2$", "^DefineDomain testvm-full$", "^dconf copy$",
		"^StartDomain testvm-full$", "^ssh sudo .*systemd-id128 new.* sh 'testvm-full'$", "systemctl poweroff",
		"^CreateSnapshot testvm-full clean$")
	if n := w.count("restart gdm"); n != 1 {
		t.Errorf("gdm restarts %d times, want once", n)
	}
	if w.count("^ssh .*omarchy update") != 0 || w.count("^SendKey") != 0 {
		t.Error("a GNOME guest got Omarchy steps")
	}
	if w.count("ShutdownDomain") != 0 {
		t.Error("provision relied on ACPI shutdown")
	}
	for _, e := range w.entries() {
		if strings.Contains(e, password) || strings.Contains(e, passphrase) {
			t.Errorf("a secret is in a command line: %q", e)
		}
	}
	for _, s := range w.shell.stdin {
		cmd, data, _ := strings.Cut(s, "\x00")
		switch {
		case strings.HasPrefix(cmd, "sudo "), strings.Contains(cmd, "unlock-keyring.js"):
			if data != password+"\n" {
				t.Errorf("%q got stdin %q, want the guest password", cmd, data)
			}
		case strings.Contains(cmd, "pinentry-program"):
			if data != passphrase+"\n" {
				t.Errorf("pinentry got stdin %q, want the passphrase", data)
			}
		}
	}
	if strings.Contains(w.out.String(), password) || strings.Contains(w.out.String(), passphrase) {
		t.Error("a secret was printed")
	}
	if len(w.fake.Devices(vm)) != 0 {
		t.Error("the stick is still attached")
	}
	desc, _ := w.fake.DomainDescription("testvm-full")
	if desc != "my notes\n"+systemd259 {
		t.Errorf("the clone description is %q", desc)
	}
}

func TestProvisionAsksEverythingBeforeItChangesAnything(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.StartDomain(vm))
	w.defineClone()
	if _, err := provision(t, w, false); err != nil {
		t.Fatalf("provision: %v", err)
	}
	w.assertSequence("checks, then questions, then secrets, then the first change",
		"^find stick /media/tester/MASTER-KEY$", "^confirm testvm is running and will be reset",
		"^confirm testvm-full exists and will be replaced",
		"^pass show vm/guest-password$", "^secret Password of tester", "^secret GPG passphrase",
		"^DestroyDomain testvm$", "^RevertSnapshot")
	w.assertSequence("the old clone goes before the new one is defined",
		"^UndefineDomain testvm-full$", "^DeleteVolume testvm-full.qcow2$", "^DeleteVolume testvm-full_VARS.qcow2$",
		"^CreateVolumeFrom testvm-full.qcow2", "^CreateVolumeFrom testvm-full_VARS.qcow2", "^DefineDomain testvm-full$")
}

// vm made the old clone and its reset points: copies of its NVRAM next to
// it. They go with the clone, as in vm's remove_full_image.
func TestProvisionDeletesTheNVRAMCopiesOfTheOldClone(t *testing.T) {
	w := newWorld(t)
	w.defineClone()
	for _, name := range []string{"testvm-full_VARS.qcow2.clean.bak", "testvm_VARS.qcow2.clean-install.bak"} {
		_, err := w.fake.CreateVolume("nvram", "<volume><name>"+name+"</name><capacity>1</capacity></volume>")
		must(t, err)
	}
	if _, err := provision(t, w, true); err != nil {
		t.Fatalf("provision: %v", err)
	}
	names, err := w.fake.Volumes("nvram")
	must(t, err)
	if slices.Contains(names, "testvm-full_VARS.qcow2.clean.bak") || !slices.Contains(names, "testvm_VARS.qcow2.clean-install.bak") {
		t.Errorf("nvram pool = %q, want only the copy of the old clone gone", names)
	}
}

func TestProvisionReplacesACloneTwice(t *testing.T) {
	w := newWorld(t)
	for range 2 {
		if _, err := provision(t, w, true); err != nil {
			t.Fatalf("provision: %v", err)
		}
	}
	if _, err := w.fake.Domain("testvm-full"); err != nil {
		t.Error(err)
	}
}

func TestProvisionRefusesACloneStartedDuringTheRun(t *testing.T) {
	w := newWorld(t)
	w.defineClone()
	w.shell.onRun = func(cmd string) {
		if strings.Contains(cmd, "bootstrap.sh") {
			must(t, w.fake.StartDomain("testvm-full"))
		}
	}
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Conflict) || !strings.Contains(err.Error(), "testvm-full is running") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^(UndefineDomain|DeleteVolume|DefineDomain) testvm-full"); i >= 0 {
		t.Errorf("the running clone was touched: %q", w.entries()[i])
	}
	if s, _ := w.fake.DomainState("testvm-full"); s != virt.StateRunning {
		t.Errorf("testvm-full is %s", s)
	}
}

func TestProvisionRefusesAnNVRAMOutsideEveryPool(t *testing.T) {
	w := newWorld(t)
	text, err := w.fake.DomainXML(vm, true)
	must(t, err)
	_, err = w.fake.DefineDomain(strings.Replace(text, "/var/lib/libvirt/qemu/nvram/", "/elsewhere/", 1))
	must(t, err)
	_, err = provision(t, w, true)
	if !failure.Is(err, failure.NotFound) || !strings.Contains(err.Error(), "is in no storage pool") {
		t.Fatalf("err = %v", err)
	}
	if w.index("^RevertSnapshot") >= 0 {
		t.Error("provision reset the build VM")
	}
}

// An NVRAM in the NVRAM directory that no pool lists can be a new file that
// the pool lists after a refresh. Preflight changes nothing, so the clone
// step finds out, before it removes the old clone.
func TestProvisionRefusesAnNVRAMNoPoolListsAfterARefresh(t *testing.T) {
	w := newWorld(t)
	w.defineClone()
	must(t, w.fake.DeleteVolume("nvram", "testvm_VARS.qcow2"))
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.NotFound) || !strings.Contains(err.Error(), "is in no storage pool") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Contains(w.fake.Calls(), "RefreshPool nvram") {
		t.Error("provision did not refresh the nvram pool")
	}
	if i := w.index("^(UndefineDomain|DeleteVolume) testvm-full"); i >= 0 {
		t.Errorf("the old clone was touched: %q", w.entries()[i])
	}
}

func TestProvisionWithoutTerminalChangesNothing(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.StartDomain(vm))
	w.term.tty = false
	_, err := provision(t, w, false)
	if !failure.Is(err, failure.NeedsConfirmation) {
		t.Fatalf("err = %v, want needs_confirmation", err)
	}
	if i := w.index("^(DestroyDomain|RevertSnapshot|StartDomain|ssh )"); i >= 0 {
		t.Errorf("provision changed something: %q", w.entries()[i])
	}
}

func TestProvisionYesWithoutTerminalNeedsASecret(t *testing.T) {
	w := newWorld(t)
	w.term.tty = false
	w.pass["vm/guest-password"] = password
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.SecretUnavailable) {
		t.Fatalf("err = %v, want secret_unavailable for the passphrase", err)
	}
	if w.index("^(RevertSnapshot|StartDomain)") >= 0 {
		t.Error("provision changed something without its secrets")
	}
}

func TestProvisionNeedsTheBase(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.DeleteSnapshot(vm, resetpoint.ProvisionBase))
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.NotFound) || !strings.Contains(err.Error(), "werft prepare testvm") {
		t.Fatalf("err = %v", err)
	}
	if len(w.entries()) > 1 {
		t.Errorf("provision did more than look for the stick: %q", w.entries())
	}
}

// GNOME opens a Files window on a plugged-in stick, and that window would
// make the unmount at the hand-over fail after the reset.
func TestProvisionRefusesABusyStickBeforeAnyQuestion(t *testing.T) {
	w := newWorld(t)
	must(t, w.fake.StartDomain(vm))
	w.defineClone()
	w.host.holders = "nautilus 1234"
	_, err := provision(t, w, false)
	if !failure.Is(err, failure.Conflict) || !strings.Contains(err.Error(), "nautilus") || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^(confirm|secret|pass show|DestroyDomain|RevertSnapshot|StartDomain|host unmount)"); i >= 0 {
		t.Errorf("a busy stick got as far as %q", w.entries()[i])
	}
}

func TestProvisionRefusesARunningClone(t *testing.T) {
	w := newWorld(t)
	w.defineClone()
	must(t, w.fake.StartDomain("testvm-full"))
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Conflict) {
		t.Fatalf("err = %v", err)
	}
	if w.index("^RevertSnapshot") >= 0 {
		t.Error("provision reset the build VM")
	}
}

func TestFailedBootstrapUndoesTheTemporaryChanges(t *testing.T) {
	w := newWorld(t)
	w.shell.fail["bootstrap.sh"] = true
	_, err := provision(t, w, true)
	if err == nil || !strings.Contains(err.Error(), "ssh -F /state/ssh_config tester@192.168.122.1") || !strings.Contains(err.Error(), "log: /state/logs/p.log") {
		t.Fatalf("err = %v, want the ssh command and the log", err)
	}
	w.assertSequence("the cleanups run newest first",
		"bootstrap.sh", "grep -vxF", "rm -f .* sh '/etc/sudoers.d/90-werft-provision'$",
		"^DetachDevice testvm$", "^host mount stick$", "mv -f .* sh '/etc/gdm3/custom.conf'$")
	if w.count("systemctl poweroff") != 0 || w.count("restart gdm") != 1 || w.count("^viewer close") != 0 {
		t.Error("the failed run did not leave the VM running with its session and its window")
	}
	if s, _ := w.fake.DomainState(vm); s != virt.StateRunning {
		t.Errorf("testvm is %s", s)
	}
}

func TestFailedChecksAreChecksFailed(t *testing.T) {
	w := newWorld(t)
	w.shell.fail["bash 'run.sh'"] = true
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.ChecksFailed) {
		t.Fatalf("err = %v", err)
	}
	if w.index("^DefineDomain") >= 0 {
		t.Error("a failed check still made the clone")
	}
}

// A dropped connection is no failed check: an agent must not read it as one.
func TestDroppedChecksAreNoChecksFailed(t *testing.T) {
	w := newWorld(t)
	w.shell.drop["bash 'run.sh'"] = true
	_, err := provision(t, w, true)
	if !failure.Is(err, failure.Failed) || !strings.Contains(err.Error(), "could not run the checks in testvm") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(w.out.String(), "checks: FAIL") {
		t.Error("a dropped connection printed checks: FAIL")
	}
}

func TestInterruptedProvisionCleansUp(t *testing.T) {
	w := newWorld(t)
	entries := steps.FromRecipe(setupRecipe(t))
	var cancel context.CancelFunc
	w.shell.onRun = func(cmd string) {
		if strings.Contains(cmd, "bootstrap.sh") {
			cancel()
		}
	}
	if _, err := w.runWithCancel(entries, &cancel); err == nil {
		t.Fatal("an interrupted run succeeded")
	}
	w.assertSequence("SIGINT pops the cleanup stack", "bootstrap.sh", "grep -vxF", "rm -f .*90-werft-provision", "^DetachDevice", "mv -f .*custom.conf")
}

func TestProvisionOmarchy(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	if _, err := provision(t, w, true); err != nil {
		t.Fatalf("provision: %v\n%s", err, w.out.String())
	}
	w.assertSequence("the disk password is typed at each boot, then the Omarchy update and its reboot",
		"^StartDomain testvm$", "^SendKey testvm \\[34\\]$", "^SendKey testvm \\[28\\]$", "^ssh true$",
		"^ssh pgrep .*'Hyprland'$", "^ssh sudo .*NOPASSWD", `\. /etc/locale\.conf && env LANG="\$LANG" .*OMARCHY_UPDATE_LOGGED=1 omarchy update -y$`,
		"systemctl poweroff", "^StartDomain testvm$", "^SendKey testvm \\[28\\]$", "^ssh rm -rf '/tmp/werft-provision'",
		"^AttachDevice", "bootstrap.sh", "rm -f .*90-werft-provision",
		"systemctl poweroff", "^StartDomain testvm$", "^SendKey testvm \\[28\\]$", "run.sh",
		"^StartDomain testvm-full$", "^SendKey testvm-full \\[28\\]$", "systemd-id128",
		"^CreateSnapshot testvm-full clean$")
	if n := w.count("NOPASSWD"); n != 1 {
		t.Errorf("sudo opens %d times, want once, before the update", n)
	}
	if w.count("AutomaticLoginEnable") != 0 {
		t.Error("Omarchy got GDM automatic login")
	}
	keys := w.fake.SentKeys(vm)
	if len(keys) != 3*(len(password)+1) {
		t.Errorf("%d keys typed at 3 boots, want %d", len(keys), 3*(len(password)+1))
	}
}

func TestProvisionRefusesAnUntypeablePassword(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	w.pass["vm/guest-password"] = "pässword"
	_, err := provision(t, w, true)
	if err == nil || !strings.Contains(err.Error(), "US keyboard") || strings.Contains(err.Error(), "ä") {
		t.Fatalf("err = %v", err)
	}
	if w.index("^(RevertSnapshot|StartDomain)") >= 0 {
		t.Error("provision changed something")
	}
}

func TestFailedCloneLeavesNoVolumes(t *testing.T) {
	w := newWorld(t)
	w.fake.FailOn("DefineDomain testvm-full", failure.New(failure.Failed, "define failed"))
	_, err := provision(t, w, true)
	if err == nil || !strings.Contains(err.Error(), "testvm itself is intact") {
		t.Fatalf("err = %v", err)
	}
	w.assertSequence("the copies go again", "^CreateVolumeFrom testvm-full.qcow2", "^CreateVolumeFrom testvm-full_VARS.qcow2",
		"^DeleteVolume testvm-full.qcow2$", "^DeleteVolume testvm-full_VARS.qcow2$")
}

// The clone inherits raw NVRAM from its build VM, so the snapshot after the
// clone is refused in preflight, not after the whole run.
func TestSnapshotOnACloneOfRawNVRAMIsRefusedFirst(t *testing.T) {
	w := newWorld(t)
	text, err := w.fake.DomainXML(vm, true)
	must(t, err)
	_, err = w.fake.DefineDomain(strings.Replace(text, `format="qcow2">/var/lib/libvirt/qemu/nvram`, `format="raw">/var/lib/libvirt/qemu/nvram`, 1))
	must(t, err)
	_, err = w.run([]pipeline.Entry{
		{Name: "clone", Step: steps.Clone{}},
		{Name: "snapshot", Step: &steps.Snapshot{Label: "clean", OnClone: true}},
	}, true)
	if !failure.Is(err, failure.Unsupported) || !strings.Contains(err.Error(), "werft migrate testvm") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^(CreateVolumeFrom|DefineDomain)"); i >= 0 {
		t.Errorf("the run got as far as %q", w.entries()[i])
	}
}

// An undefine with the NVRAM would unlink the file behind its pool, and the
// new copy under that name would fail: a pool error stops the removal.
func TestPoolErrorKeepsTheOldClone(t *testing.T) {
	w := newWorld(t)
	w.defineClone()
	w.fake.FailOn("VolumeByPath /var/lib/libvirt/qemu/nvram/testvm-full_VARS.qcow2", failure.New(failure.NotFound, "no volume"))
	w.fake.FailOn("Pools", failure.New(failure.Failed, "pool list failed"))
	_, err := w.run([]pipeline.Entry{{Name: "clone", Step: steps.Clone{}}}, true)
	if err == nil || !strings.Contains(err.Error(), "pool list failed") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^UndefineDomain testvm-full$"); i >= 0 {
		t.Error("the old clone was undefined")
	}
}

// libvirt copies the disk of a running guest without a word; the copy would
// be inconsistent.
func TestCloneRefusesARunningSource(t *testing.T) {
	w := newWorld(t)
	w.defineClone()
	must(t, w.fake.StartDomain(vm))
	_, err := w.run([]pipeline.Entry{{Name: "clone", Step: steps.Clone{}}}, true)
	if !failure.Is(err, failure.Conflict) || !strings.Contains(err.Error(), "testvm is running") || !strings.Contains(err.Error(), "poweroff") {
		t.Fatalf("err = %v", err)
	}
	if i := w.index("^(UndefineDomain|DeleteVolume|CreateVolumeFrom|DefineDomain)"); i >= 0 {
		t.Errorf("clone acted on a running source: %q", w.entries()[i])
	}
}
