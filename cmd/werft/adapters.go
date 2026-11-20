package main

import (
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/steps"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virt/libvirt"
)

// guestDir is where upload puts files in a guest. A boot empties it.
const guestDir = "/tmp/werft-provision"

// buildVMRun wires the real adapters into a run on build VM name. It takes
// the domain locks of name and of its clone, and writes the werft
// ssh_config. close releases what it holds.
func (c *cli) buildVMRun(name string, yes bool, out io.Writer) (r *pipeline.Run, close func(), err error) {
	h, err := c.hypervisor()
	if err != nil {
		return nil, nil, err
	}
	var closers []func()
	close = func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	closers = append(closers, func() { _ = h.Close() })
	fail := func(err error) (*pipeline.Run, func(), error) {
		close()
		return nil, nil, err
	}
	if _, err := h.Domain(name); err != nil {
		return fail(err)
	}
	dir, err := state.Root()
	if err != nil {
		return fail(err)
	}
	for _, d := range []string{name, name + resetpoint.FullSuffix} {
		lock, err := dir.LockDomain(d)
		if err != nil {
			return fail(err)
		}
		closers = append(closers, func() { _ = lock.Release() })
	}
	u, err := user.Current()
	if err != nil {
		return fail(err)
	}
	home, _ := os.UserHomeDir()
	userKey := filepath.Join(home, ".ssh", "id_ed25519")
	if err := guest.WriteConfig(dir.SSHConfig(), dir.SSHKey(), userKey); err != nil {
		return fail(failure.Wrap(failure.Failed, err, "write %s", dir.SSHConfig()))
	}
	r = (&pipeline.Run{
		H:         h,
		Shell:     guest.OpenSSH{Config: dir.SSHConfig()},
		KeyShell:  guest.OpenSSH{Config: dir.SSHConfig(), KeyOnly: true},
		Host:      steps.System{User: u.Username, Out: out},
		Secrets:   c.secretStore(),
		Prompter:  prompter{c},
		Out:       out,
		Yes:       yes,
		BuildVM:   name,
		Domain:    name,
		User:      u.Username,
		GuestDir:  guestDir,
		SSHConfig: dir.SSHConfig(),
		SSHKey:    dir.SSHKey(),
		UserKey:   userKey,
		Addr:      guest.Addr{User: u.Username},
	}).Init()
	return r, close, nil
}

func dialLibvirt() (virt.Hypervisor, error) { return libvirt.Dial() }

// rootShell is a shell that logs in as root with a new key pair, and the
// public half of the key. The seal gives it to the guest as a credential.
// cleanup removes the key.
func rootShell() (sh guest.Shell, pubkey string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "werft-seal-")
	if err != nil {
		return nil, "", nil, failure.Wrap(failure.Failed, err, "create a directory for the seal key")
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	key, config := filepath.Join(tmp, "id_ed25519"), filepath.Join(tmp, "ssh_config")
	if err := guestEnsureKey(key); err != nil {
		cleanup()
		return nil, "", nil, failure.Wrap(failure.Failed, err, "create the seal key")
	}
	pub, err := os.ReadFile(key + ".pub")
	if err == nil {
		err = guest.WriteConfig(config, key)
	}
	if err != nil {
		cleanup()
		return nil, "", nil, failure.Wrap(failure.Failed, err, "set up the seal key")
	}
	return guest.OpenSSH{Config: config}, strings.TrimSpace(string(pub)), cleanup, nil
}

// openViewer opens virt-viewer in a session of its own, so the window
// outlives werft. Over SSH to the host there is no display, and the viewer
// would fail without a word.
func openViewer(domain string) error {
	host := steps.System{}
	if !host.HasDisplay() {
		return errors.New("no display here")
	}
	_, err := host.OpenViewer(domain, false)
	return err
}

var guestEnsureKey = guest.EnsureKey

// secretStore is the keyring and pass of the host, unless a test set
// c.secrets.
func (c *cli) secretStore() secret.Store {
	if c.secrets == nil {
		return secret.Host{}
	}
	return c.secrets
}

// prompter asks at the terminal of the cli.
type prompter struct{ c *cli }

func (p prompter) Confirm(question string) error { return p.c.confirm(false, question) }

func (p prompter) HasTerminal() bool { return p.c.tty }

func (p prompter) ReadSecret(prompt string) (string, error) {
	if !p.c.tty {
		return "", failure.New(failure.SecretUnavailable, "%s: stdin is no terminal to ask on", prompt)
	}
	_, _ = io.WriteString(p.c.stderr, prompt+": ")
	v, err := p.c.readSecret()
	_, _ = io.WriteString(p.c.stderr, "\n")
	if err != nil {
		return "", failure.Wrap(failure.SecretUnavailable, err, "%s", prompt)
	}
	return v, nil
}
