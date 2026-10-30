package steps

import (
	"bytes"
	"os"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
)

// Prepare is the fixed step list of werft prepare: it turns a stock install
// into the base provision starts from. What only a person at the guest can
// do is said, and the run waits for it.
func Prepare() []pipeline.Entry {
	w := &window{}
	return []pipeline.Entry{
		{Name: "boot", Step: &PrepareBoot{window: w}},
		{Name: "wait-ssh", Step: &PrepareSSH{window: w}},
		{Name: "detect-guest", Step: DetectGuest{}},
		{Name: "autologin", Family: "ubuntu", Step: &Autologin{Prepare: true}},
		{Name: "verify-guest-password", Step: VerifyGuestPassword{}},
		{Name: "luks-mark", Step: LUKSMark{}},
		{Name: "hostname", Step: Hostname{}},
		{Name: "poweroff", Step: Poweroff{}},
		{Name: "snapshot", Step: &Snapshot{Label: resetpoint.ProvisionBase}},
	}
}

// window is the viewer a person acts in, opened once however many steps
// need them. It stays open after prepare.
type window struct{ shown bool }

func (w *window) show(r *pipeline.Run) {
	if w.shown {
		return
	}
	w.shown = true
	if r.Host.HasDisplay() {
		if _, err := r.Host.OpenViewer(r.Domain, false); err == nil {
			return
		}
	}
	r.Printf("  Open the window on the desktop: virt-viewer -c qemu:///system %s", r.Domain)
}

// PrepareBoot starts the domain and waits for its address. An encrypted
// disk holds the guest in its initramfs, with no network, until a person
// types its password; prepare never types it.
type PrepareBoot struct{ window *window }

func (PrepareBoot) Check(*pipeline.Run) error { return nil }

func (p *PrepareBoot) Run(r *pipeline.Run) error {
	if !r.Running() {
		if err := r.H.StartDomain(r.Domain); err != nil {
			return err
		}
		r.Printf("%s started", r.Domain)
	}
	hasAddress := func() bool { return len(r.Candidates()) > 0 }
	if err := r.Poll(addressTimeout, pollInterval, "", hasAddress); !failure.Is(err, failure.Timeout) {
		return err
	}
	r.Printf("\n%s has no address yet. If its disk asks for a password, type it in its window.\n  The run continues once it has an address.\n", r.Domain)
	p.window.show(r)
	return r.Poll(personTimeout, pollInterval, r.Domain+" took no address", hasAddress)
}

// PrepareSSH waits for sshd, with the install commands for a guest that has
// none, and installs the werft key when the guest does not accept it. One
// login attempt with the werft key alone says both whether sshd is up and
// whether it knows the key: "Permission denied" comes from sshd itself.
type PrepareSSH struct{ window *window }

func (PrepareSSH) Check(*pipeline.Run) error { return nil }

func (p *PrepareSSH) Run(r *pipeline.Run) error {
	accepted := false
	answers := func() bool {
		for _, a := range r.Candidates() {
			ok, answered := sshReply(r, a.Host)
			if answered {
				r.Addr, accepted = a, ok
				return true
			}
		}
		return false
	}
	if err := r.PollLogin(sshdBootTimeout, "", answers); err != nil {
		if !failure.Is(err, failure.Timeout) {
			return err
		}
		r.Printf("\n%s has no SSH server. Log in at its desktop, open a terminal and run:\n\n      sudo apt install -y openssh-server                         (Ubuntu)\n      sudo ufw allow ssh && sudo systemctl enable --now sshd     (Omarchy)\n\n  The run continues once SSH answers.\n", r.Domain)
		p.window.show(r)
		if err := r.PollLogin(personTimeout, "no SSH server answered in "+r.Domain, answers); err != nil {
			return err
		}
	}
	r.Printf("SSH server answers in %s", r.Domain)
	if !accepted {
		if err := installKey(r); err != nil {
			return err
		}
		if ok, _ := sshReply(r, r.Addr.Host); !ok {
			return failure.New(failure.Failed, "%s still rejects the werft key", r.Domain)
		}
	}
	r.Printf("%s accepts the werft key", r.Domain)
	return nil
}

// appendKey adds the key on stdin to authorized_keys, on a line of its own.
const appendKey = `umask 077 && mkdir -p ~/.ssh && f=~/.ssh/authorized_keys && { if [ -s "$f" ] && [ -n "$(tail -c 1 "$f")" ]; then echo; fi; cat; } >>"$f"`

// installKey adds the werft key over a login with the user's own key, so no
// password is asked for, and else with ssh-copy-id.
func installKey(r *pipeline.Run) error {
	if r.Probe("true") {
		r.Printf("Adding the werft key to %s over the login with your own key", r.Domain)
		pub, err := os.ReadFile(r.SSHKey + ".pub")
		if err != nil {
			return failure.Wrap(failure.Failed, err, "read the werft key")
		}
		if err := r.Exec(appendKey, bytes.NewReader(pub)); err != nil {
			return failure.Wrap(failure.Failed, err, "could not add the werft key to ~/.ssh/authorized_keys in %s", r.Domain)
		}
		return nil
	}
	r.Printf("Copying the werft key into %s; ssh asks for the password of %s in the VM %s, as %s", r.Domain, r.User, r.Domain, r.Addr)
	if err := r.Host.CopySSHKey(r.Addr, r.SSHKey+".pub"); err != nil {
		return failure.Wrap(failure.Failed, err, "could not copy the SSH key into %s; ssh-copy-id says why, above; the user in the VM must be %s", r.Domain, r.User)
	}
	return nil
}
