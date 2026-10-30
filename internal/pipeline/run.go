package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/hostdev"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virt"
)

// GuestPassword is the secret of the guest user's password. sudo in the
// guest needs it, and an encrypted disk asks for it at boot.
const GuestPassword = "guest-password"

// Prompter asks the questions of a run at the terminal. Confirm fails with
// failure.NeedsConfirmation when the answer is no or there is no terminal.
type Prompter interface {
	Confirm(question string) error
	secret.Terminal
}

// Host is what steps do on the host, outside libvirt.
type Host interface {
	HasDisplay() bool
	// OpenViewer opens a detached viewer window on domain; stop closes it.
	OpenViewer(domain string, reconnect bool) (stop func(), err error)
	CopyViewerSettings(fromUUID, toUUID string) error
	StickMount(label string) string
	FindStick(mount string) (hostdev.Stick, error)
	StickHolders(s hostdev.Stick) string
	UnmountStick(s hostdev.Stick) error
	MountStick(s hostdev.Stick) error
	StickOnHost(s hostdev.Stick) bool
	// CopySSHKey installs pubkey for addr with ssh-copy-id, which asks for
	// the guest password at the terminal.
	CopySSHKey(addr guest.Addr, pubkey string) error
}

// Run is the context the steps of one run share.
type Run struct {
	Ctx   context.Context
	H     virt.Hypervisor
	Shell guest.Shell
	// KeyShell logs in with the werft key alone; prepare asks it whether the
	// guest accepts the key.
	KeyShell guest.Shell
	Host     Host
	Secrets  secret.Store
	Prompter Prompter
	// Out gets progress and guest output: stderr and the log file.
	Out io.Writer
	// Yes answers every question with yes.
	Yes   bool
	Sleep func(time.Duration)

	// BuildVM is the domain the run is for; Domain is the one the steps act
	// on, the clone after the clone step.
	BuildVM string
	Domain  string
	User    string
	// GuestDir is where upload puts files in the guest.
	GuestDir  string
	SSHConfig string
	// SSHKey is the private key prepare installs in the guest.
	SSHKey  string
	LogPath string
	// PasswordSpec says where the guest password comes from.
	PasswordSpec secret.Spec

	// Addr is the guest login that answered last.
	Addr           guest.Addr
	Family         string
	SessionProcess string
	// LUKS is set when the domain's disk asks for a password at boot.
	LUKS bool

	questions  []string
	specs      map[string]secret.Spec
	order      []string
	validators map[string][]func(string) error
	values     map[string]string
	stack      []pushed
	viewer     func()
}

// Init makes r ready for Execute.
func (r *Run) Init() *Run {
	r.specs = map[string]secret.Spec{}
	r.validators = map[string][]func(string) error{}
	r.values = map[string]string{}
	if r.Ctx == nil {
		r.Ctx = context.Background()
	}
	if r.Sleep == nil {
		r.Sleep = r.pause
	}
	return r
}

func (r *Run) Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.Out, format+"\n", args...)
}

// Ask queues a question. Execute asks the queued questions once every check
// passed, unless the run answers yes to all.
func (r *Run) Ask(question string) {
	r.questions = append(r.questions, question)
}

func (r *Run) askQuestions() error {
	if r.Yes {
		return nil
	}
	for _, q := range r.questions {
		if err := r.Prompter.Confirm(q); err != nil {
			return err
		}
	}
	return nil
}

// SetViewer keeps the stop function of the open viewer window.
func (r *Run) SetViewer(stop func()) { r.viewer = stop }

// CloseViewer closes the viewer window, if one is open.
func (r *Run) CloseViewer() {
	if r.viewer != nil {
		r.viewer()
		r.viewer = nil
	}
}

// pause sleeps for d, and ends early when the run is interrupted.
func (r *Run) pause(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.Ctx.Done():
	case <-t.C:
	}
}

// Poll calls cond every interval until it is true. It fails with
// failure.Timeout, "<what> within <timeout>", once timeout has passed, and
// when the run is interrupted.
func (r *Run) Poll(timeout, interval time.Duration, what string, cond func() bool) error {
	return r.poll(timeout, func(int) time.Duration { return interval }, what, cond)
}

// PollLogin is Poll for SSH logins: the pauses between the attempts grow
// as guest.LoginPause says.
func (r *Run) PollLogin(timeout time.Duration, what string, cond func() bool) error {
	return r.poll(timeout, guest.LoginPause, what, cond)
}

func (r *Run) poll(timeout time.Duration, pause func(attempt int) time.Duration, what string, cond func() bool) error {
	waited := time.Duration(0)
	for attempt := 1; ; attempt++ {
		if err := r.Ctx.Err(); err != nil {
			return failure.Wrap(failure.Failed, err, "interrupted while waiting: %s", what)
		}
		if cond() {
			return nil
		}
		if waited >= timeout {
			return failure.New(failure.Timeout, "%s within %v", what, timeout)
		}
		d := min(pause(attempt), timeout-waited)
		r.Sleep(d)
		waited += d
	}
}

// Probe runs cmd at r.Addr and reports whether it succeeded. Its output is
// dropped.
func (r *Run) Probe(cmd string) bool {
	return r.Shell.Run(r.Ctx, r.Addr, cmd, nil, io.Discard, io.Discard) == nil
}

// Exec runs cmd at r.Addr with its output in r.Out.
func (r *Run) Exec(cmd string, stdin io.Reader) error {
	return r.Shell.Run(r.Ctx, r.Addr, cmd, stdin, r.Out, r.Out)
}

// Output runs cmd at r.Addr and gives its stdout.
func (r *Run) Output(cmd string, stdin io.Reader) (string, error) {
	var out bytes.Buffer
	err := r.Shell.Run(r.Ctx, r.Addr, cmd, stdin, &out, r.Out)
	return strings.TrimSpace(out.String()), err
}

// Root runs script as root, with the guest password for sudo on stdin.
func (r *Run) Root(script string, args ...string) error {
	return r.Exec(guest.RootCommand(script, args...), guest.Line(r.Secret(GuestPassword)))
}

// SSHCommand is the ssh command line that reaches r.Addr.
func (r *Run) SSHCommand() string {
	return guest.OpenSSH{Config: r.SSHConfig}.Command(r.Addr)
}

// Candidates are the logins that can reach the domain: vsock first, then the
// DHCP lease of each MAC. A reset guest keeps its MAC, so a lease can name
// the address before sshd is up: only an SSH probe tells.
func (r *Run) Candidates() []guest.Addr {
	cid, ips := virt.DomainAddresses(r.H, r.Domain)
	var out []guest.Addr
	if cid != "" {
		out = append(out, guest.Addr{User: r.User, Host: cid})
	}
	for _, ip := range ips {
		out = append(out, guest.Addr{User: r.User, Host: ip})
	}
	return out
}

// Reach sets r.Addr to the first candidate where cmd succeeds, and reports
// whether one did.
func (r *Run) Reach(cmd string) bool {
	for _, a := range r.Candidates() {
		if r.Shell.Run(r.Ctx, a, cmd, nil, io.Discard, io.Discard) == nil {
			r.Addr = a
			return true
		}
	}
	return false
}

// Running reports whether the domain is running.
func (r *Run) Running() bool {
	s, err := r.H.DomainState(r.Domain)
	return err == nil && s == virt.StateRunning
}
