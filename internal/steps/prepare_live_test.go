//go:build live

package steps_test

import (
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/steps"
)

// TestPrepareKeysLive runs the boot and wait-ssh steps of prepare on a guest
// that accepts the user's own key and not the werft key, as a build VM vm
// prepared does: ssh-copy-id put the user's key in authorized_keys.
// Afterwards authorized_keys holds both keys once, a second run adds
// nothing, and the werft key alone logs in.
func TestPrepareKeysLive(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-pg-keys")
	werftKey := livetest.NewKey(t)
	config := filepath.Join(t.TempDir(), "ssh_config")
	livetest.Must(t, guest.WriteConfig(config, werftKey.Private, g.Key.Private), "write ssh_config")
	g.Boot()
	g.Run("install -d -m 700 /root/.ssh && echo '" + g.Key.Public + "' > /root/.ssh/authorized_keys")
	g.Poweroff()
	bootAndSSH := stepsNamed(t, steps.Prepare(), "boot", "wait-ssh")

	newRun := func() *pipeline.Run {
		return (&pipeline.Run{
			H: h, Shell: guest.OpenSSH{Config: config}, KeyShell: guest.OpenSSH{Config: config, KeyOnly: true},
			Host: noHost{}, Prompter: noPrompter{}, Out: testWriter{t}, Yes: true,
			BuildVM: g.Name, Domain: g.Name, User: "root", SSHConfig: config,
			SSHKey: werftKey.Private, UserKey: g.Key.Private, Addr: guest.Addr{User: "root"},
		}).Init()
	}
	var r *pipeline.Run
	for range 2 {
		r = newRun()
		if err := pipeline.Execute(r, bootAndSSH); err != nil {
			t.Fatalf("prepare: %v\nconsole:\n%s", err, livetest.ConsoleTail(h, g.ConsoleVolume()))
		}
	}
	got, err := r.Output("cat /root/.ssh/authorized_keys", nil)
	livetest.Must(t, err, "read authorized_keys")
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	for _, key := range []string{werftKey.Public, g.Key.Public} {
		if n := slices.Index(lines, key); n < 0 || slices.Contains(lines[n+1:], key) {
			t.Errorf("authorized_keys holds a key not exactly once:\n%s\nkey: %s", got, key)
		}
	}
	if len(lines) != 2 {
		t.Errorf("authorized_keys =\n%s\nwant the two keys only", got)
	}
	err = guest.OpenSSH{Config: config, KeyOnly: true}.Run(r.Ctx, r.Addr, "true", nil, io.Discard, io.Discard)
	livetest.Must(t, err, "log in with the werft key alone")
	if strings.Contains(got, "\n\n") {
		t.Error("authorized_keys has an empty line")
	}
}

// stepsNamed is the entries of all with the names, in that order.
func stepsNamed(t *testing.T, all []pipeline.Entry, names ...string) []pipeline.Entry {
	t.Helper()
	var out []pipeline.Entry
	for _, name := range names {
		i := slices.IndexFunc(all, func(e pipeline.Entry) bool { return e.Name == name })
		if i < 0 {
			t.Fatalf("no step %s", name)
		}
		out = append(out, all[i])
	}
	return out
}
