//go:build live

package steps_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/hostdev"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/recipe"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
)

const liveRecipe = `
[[steps]]
type = "reset"
label = "base"

[[steps]]
type = "boot"

[[steps]]
type = "wait-ssh"

[[steps]]
type = "upload"
files = ["mark.sh"]

[[steps]]
type = "run"
command = "bash mark.sh"

[[steps]]
type = "checks"
dir = "checks"

[[steps]]
type = "poweroff"

[[steps]]
type = "clone"

[[steps]]
type = "generalize"

[[steps]]
type = "snapshot"
label = "clean"
`

// The recipe's own check: the helpers and the recipe file are in the guest
// directory, the run step ran, and the reset took away what the control step
// wrote after the reset point.
const liveChecks = `set -e
test -x /tmp/werft-provision/pinentry-auto
test -f /tmp/werft-provision/mark.sh
test -f /root/werft-ran
test ! -e /root/stale
echo "PASS  live"
`

// TestProvisionLive runs a provision recipe on a scratch domain made from the
// cloud image: reset, boot, wait-ssh over vsock, upload, run, checks,
// poweroff, clone, generalize and snapshot. The desktop and hardware steps
// need a desktop guest and a key stick, so they are left out.
func TestProvisionLive(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-pp-prov")
	clone := g.Name + resetpoint.FullSuffix
	removeClone(t, h, clone)
	t.Cleanup(func() {
		if !t.Failed() || os.Getenv("WERFT_TEST_KEEP") == "" {
			removeClone(t, h, clone)
		}
	})

	livetest.Must(t, resetpoint.Take(h, g.Name, "base", false), "take the reset point")
	// Control: a file written after the reset point is there until the reset.
	g.Boot()
	g.Run("touch /root/stale && test -e /root/stale && sync")
	g.Poweroff()

	dir := t.TempDir()
	livetest.Must(t, os.WriteFile(filepath.Join(dir, "mark.sh"), []byte("touch /root/werft-ran\necho machine-id=$(cat /etc/machine-id)\n"), 0o644), "write mark.sh")
	livetest.Must(t, os.MkdirAll(filepath.Join(dir, "checks"), 0o755), "make checks")
	livetest.Must(t, os.WriteFile(filepath.Join(dir, "checks", "run.sh"), []byte(liveChecks), 0o755), "write run.sh")
	livetest.Must(t, os.WriteFile(filepath.Join(dir, "recipe.toml"), []byte(liveRecipe), 0o644), "write recipe")
	rec, err := recipe.Load(filepath.Join(dir, "recipe.toml"))
	livetest.Must(t, err, "load recipe")

	config := filepath.Join(dir, "ssh_config")
	livetest.Must(t, guest.WriteConfig(config, g.Key.Private), "write ssh_config")
	var out bytes.Buffer
	r := (&pipeline.Run{
		H: h, Shell: guest.OpenSSH{Config: config}, Host: noHost{}, Prompter: noPrompter{},
		Out: io.MultiWriter(&out, testWriter{t}), Yes: true,
		BuildVM: g.Name, Domain: g.Name, User: "root", GuestDir: "/tmp/werft-provision",
		SSHConfig: config, Addr: guest.Addr{User: "root"},
	}).Init()
	if err := pipeline.Execute(r, steps.FromRecipe(rec)); err != nil {
		t.Fatalf("provision: %v\nconsole:\n%s", err, livetest.ConsoleTail(h, g.ConsoleVolume()))
	}
	if !strings.Contains(out.String(), "checks: PASS") {
		t.Error("the checks did not pass")
	}
	if r.Domain != clone {
		t.Errorf("the run ends on %s, not on %s", r.Domain, clone)
	}

	for name, want := range map[string]virt.State{g.Name: virt.StateShutOff, clone: virt.StateShutOff} {
		if s, err := h.DomainState(name); err != nil || s != want {
			t.Errorf("%s is %v, %v; want %v", name, s, err, want)
		}
	}
	if ok, err := resetpoint.Has(h, clone, "clean"); err != nil || !ok {
		t.Errorf("%s has no reset point 'clean': %v", clone, err)
	}

	// The clone carries the disk of the build VM, with its own identity.
	buildID := regexp.MustCompile(`machine-id=(\w+)`).FindStringSubmatch(out.String())
	if buildID == nil {
		t.Fatalf("no machine-id of %s in the output", g.Name)
	}
	livetest.Must(t, h.StartDomain(clone), "start "+clone)
	r.Domain = clone
	if err := r.Poll(livetestBoot, sshPoll, "no SSH from "+clone, func() bool { return r.Reach("true") }); err != nil {
		t.Fatal(err)
	}
	got, err := r.Output("cat /etc/machine-id; hostname; test -f /root/werft-ran && echo ran", nil)
	livetest.Must(t, err, "read the identity of "+clone)
	lines := strings.Fields(got)
	if len(lines) != 3 || lines[0] == buildID[1] || lines[1] != clone || lines[2] != "ran" {
		t.Errorf("%s: machine-id, hostname, marker = %q; build VM machine-id %s", clone, lines, buildID[1])
	}
	livetest.Must(t, h.DestroyDomain(clone), "stop "+clone)

	// A second provision replaces the clone. Its NVRAM copy takes the name
	// of the old one, which the pool must not hold any more.
	again := (&pipeline.Run{
		H: h, Shell: r.Shell, Host: noHost{}, Prompter: noPrompter{}, Out: testWriter{t}, Yes: true,
		BuildVM: g.Name, Domain: g.Name, User: "root", SSHConfig: config,
	}).Init()
	if err := pipeline.Execute(again, []pipeline.Entry{{Name: "clone", Step: steps.Clone{}}}); err != nil {
		t.Fatalf("replace %s: %v", clone, err)
	}
	if _, err := h.Domain(clone); err != nil {
		t.Errorf("no %s after the replace: %v", clone, err)
	}
}

// removeClone removes the clone a provision run made, with its NVRAM and
// its disk volume. It refuses a name without the live-test prefix.
func removeClone(t *testing.T, h virt.Hypervisor, name string) {
	t.Helper()
	if !strings.HasPrefix(name, livetest.DomainPrefix) {
		t.Fatalf("clone %q does not start with %q", name, livetest.DomainPrefix)
	}
	_ = h.DestroyDomain(name)
	_ = h.UndefineDomain(name, virt.UndefineSnapshots|virt.UndefineNVRAM)
	livetest.DeleteVolumes(h, name+".qcow2", name+"_VARS.qcow2")
}

type noHost struct{}

func (noHost) HasDisplay() bool                        { return false }
func (noHost) OpenViewer(string, bool) (func(), error) { return func() {}, nil }
func (noHost) CopyViewerSettings(string, string) error { return nil }
func (noHost) StickMount(string) string                { return "" }
func (noHost) FindStick(string) (hostdev.Stick, error) { return hostdev.Stick{}, nil }
func (noHost) StickHolders(hostdev.Stick) string       { return "" }
func (noHost) UnmountStick(hostdev.Stick) error        { return nil }
func (noHost) MountStick(hostdev.Stick) error          { return nil }
func (noHost) StickOnHost(hostdev.Stick) bool          { return true }
func (noHost) CopySSHKey(guest.Addr, string) error     { return nil }

type noPrompter struct{}

func (noPrompter) Confirm(string) error              { return nil }
func (noPrompter) ReadSecret(string) (string, error) { return "root needs none", nil }

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

const (
	livetestBoot = 3 * time.Minute
	sshPoll      = 5 * time.Second
)
