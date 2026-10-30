//go:build live

package steps_test

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/steps"
)

// TestPrepareKeysLive runs the boot and wait-ssh steps of prepare on a guest
// that accepts the user's own key and not the werft key, as a build VM vm
// prepared does. Afterwards authorized_keys holds both keys once, a second
// run adds nothing, and the werft key alone logs in.
func TestPrepareKeysLive(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-pg-keys")
	werftKey := livetest.NewKey(t)
	config := filepath.Join(t.TempDir(), "ssh_config")
	livetest.Must(t, guest.WriteConfig(config, werftKey.Private, g.Key.Private), "write ssh_config")

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
		if err := pipeline.Execute(r, steps.Prepare()[:2]); err != nil {
			t.Fatalf("prepare: %v\nconsole:\n%s", err, livetest.ConsoleTail(h, g.ConsoleVolume()))
		}
	}
	got, err := r.Output("cat /root/.ssh/authorized_keys", nil)
	livetest.Must(t, err, "read authorized_keys")
	if want := werftKey.Public + "\n" + g.Key.Public; got != want {
		t.Errorf("authorized_keys =\n%s\nwant\n%s", got, want)
	}
	err = guest.OpenSSH{Config: config, KeyOnly: true}.Run(r.Ctx, r.Addr, "true", nil, io.Discard, io.Discard)
	livetest.Must(t, err, "log in with the werft key alone")
	if strings.Contains(got, "\n\n") {
		t.Error("authorized_keys has an empty line")
	}
}
