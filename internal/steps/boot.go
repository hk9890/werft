// Package steps holds the built-in steps of prepare and provision
// (docs/DESIGN.md, "Built-in steps"), and the fixed step list of prepare.
package steps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/console"
	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/virt"
)

// Timeouts. The person timeouts wait for someone at the guest's window.
const (
	sshTimeout       = 300 * time.Second
	sessionTimeout   = 180 * time.Second
	mountTimeout     = 60 * time.Second
	shutdownTimeout  = 120 * time.Second
	addressTimeout   = 60 * time.Second
	sshdBootTimeout  = 60 * time.Second
	personTimeout    = 3600 * time.Second
	stickBackTimeout = 15 * time.Second
	pollInterval     = 3 * time.Second
)

func hasLUKSMarker(r *pipeline.Run, domain string) bool {
	desc, err := r.H.DomainDescription(domain)
	return err == nil && domxml.HasLUKSMarker(desc)
}

// declareDiskPassword declares the guest password, which must be typeable,
// when the build VM's disk asks for it at boot.
func declareDiskPassword(r *pipeline.Run) {
	if hasLUKSMarker(r, r.BuildVM) {
		r.Declare(pipeline.GuestPassword, r.PasswordSpec, console.Typeable)
	}
}

func declarePassword(r *pipeline.Run) {
	r.Declare(pipeline.GuestPassword, r.PasswordSpec)
}

// Reset forces off the build VM and reverts it to a reset point.
type Reset struct{ Label string }

func (s Reset) label() string {
	if s.Label == "" {
		return resetpoint.ProvisionBase
	}
	return s.Label
}

func (s Reset) Check(r *pipeline.Run) error {
	if err := resetpoint.Guard(r.H, r.Domain); err != nil {
		return err
	}
	ok, err := resetpoint.Has(r.H, r.Domain, s.label())
	if err != nil {
		return err
	}
	if !ok {
		return failure.New(failure.NotFound, "%s has no reset point '%s'; take it with: werft prepare %s", r.Domain, s.label(), r.Domain)
	}
	if state, err := r.H.DomainState(r.Domain); err != nil || state == virt.StateShutOff {
		return err
	}
	r.Ask(r.Domain + " is running and will be reset to '" + s.label() + "'. Everything in it is lost.")
	return nil
}

func (s Reset) Run(r *pipeline.Run) error {
	off, err := shutOff(r)
	if err != nil {
		return err
	}
	if !off {
		r.Printf("Forcing %s off", r.Domain)
		if err := r.H.DestroyDomain(r.Domain); err != nil {
			return err
		}
	}
	r.Printf("Resetting %s to '%s'", r.Domain, s.label())
	return resetpoint.Revert(r.H, r.Domain, s.label())
}

func shutOff(r *pipeline.Run) (bool, error) {
	s, err := r.H.DomainState(r.Domain)
	if err != nil {
		return false, failure.Wrap(failure.KindOf(err), err, "could not read the state of %s", r.Domain)
	}
	return s == virt.StateShutOff, nil
}

// Boot starts the domain. An encrypted guest takes no address before its
// disk is open, so the password is typed first, once: keys sent before the
// prompt are lost, and cryptsetup allows few tries. While it waits for the
// prompt it probes SSH: a person can open the disk in a viewer, and a
// desktop that moves never holds still. A domain that runs already gets no
// keys: its screen can be a desktop, and the password would go to the
// window that has focus.
type Boot struct{}

func (Boot) Check(r *pipeline.Run) error {
	declareDiskPassword(r)
	return nil
}

func (Boot) Run(r *pipeline.Run) error { return boot(r) }

func boot(r *pipeline.Run) error {
	running, err := r.Running()
	if err != nil {
		return err
	}
	if running {
		r.LUKS = false
		if hasLUKSMarker(r, r.Domain) {
			r.Printf("%s runs already, so werft types no disk password; if its console asks for it, type it there: %s", r.Domain, ViewerCommand(r.Domain))
		}
		return nil
	}
	if err := r.H.StartDomain(r.Domain); err != nil {
		return err
	}
	r.Printf("%s started (headless, no window)", r.Domain)
	r.LUKS = hasLUKSMarker(r, r.Domain)
	if !r.LUKS {
		return nil
	}
	if r.Host.EarlierViewer(r.Domain) {
		r.Printf("warning: a viewer from an earlier run is open on %s; type nothing in it while werft waits for the disk prompt", r.Domain)
	}
	err = typeDiskPassword(r.Ctx, r.H, r.Domain, r.Secret(pipeline.GuestPassword), r.Sleep, r.Out, func() bool { return r.Reach("true") })
	switch {
	case errors.Is(err, console.ErrOpened):
		r.LUKS = false
		r.Printf("The disk of %s was opened already, by hand; werft typed nothing", r.Domain)
		return nil
	case failure.Is(err, failure.Timeout):
		if _, serr := r.Running(); serr != nil {
			return failure.Wrap(failure.KindOf(serr), serr, "waiting for the disk prompt")
		}
		return failure.Wrap(failure.Timeout, err, "look at its console: %s; if the console shows a desktop, the disk was opened by hand; close the old viewer windows and run again", ViewerCommand(r.Domain))
	}
	return err
}

// TypeDiskPassword waits for the disk password prompt at the console of the
// started domain and types password once: keys sent before the prompt are
// lost, and cryptsetup allows few tries. Seal and lease use it too. out
// gets progress, never the password. The error names no viewer: seal and
// lease remove the domain after a failure.
func TypeDiskPassword(ctx context.Context, h virt.Hypervisor, domain, password string, sleep func(time.Duration), out io.Writer) error {
	return typeDiskPassword(ctx, h, domain, password, sleep, out, nil)
}

// typeDiskPassword is TypeDiskPassword that also asks opened, when set,
// whether the disk is open already, and then types nothing and returns
// console.ErrOpened.
func typeDiskPassword(ctx context.Context, h virt.Hypervisor, domain, password string, sleep func(time.Duration), out io.Writer, opened func() bool) error {
	_, _ = fmt.Fprintf(out, "Waiting for the disk password prompt of %s\n", domain)
	p := console.DefaultPrompt
	p.Sleep = sleep
	p.Opened = opened
	if err := console.WaitForPrompt(ctx, h, domain, p); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Typing the disk password of %s at its console\n", domain)
	return console.Type(h, domain, password)
}

// WaitSSH waits until sshd in the guest answers and sets the address.
type WaitSSH struct{}

func (WaitSSH) Check(*pipeline.Run) error { return nil }

func (WaitSSH) Run(r *pipeline.Run) error { return waitSSH(r) }

func waitSSH(r *pipeline.Run) error {
	err := r.PollLogin(sshTimeout, "no SSH answer from "+r.Domain, func() bool { return r.Reach("true") })
	if err != nil && r.LUKS {
		return failure.Wrap(failure.KindOf(err), err, "look at its console (%s): an empty disk password prompt means the password went to a screen before the prompt; a prompt again after a 'wrong' or 'No key available' line means the disk refused it; nothing types it twice", ViewerCommand(r.Domain))
	}
	if err == nil {
		r.Printf("SSH answers in %s at %s", r.Domain, r.Addr.Host)
	}
	return err
}

// Poweroff powers the guest off from inside. ACPI goes through the desktop
// session, which may answer with a dialog nobody sees, and Omarchy ignores
// it. Inhibitors are skipped for the same reason.
type Poweroff struct{}

func (Poweroff) Check(r *pipeline.Run) error {
	declarePassword(r)
	return nil
}

func (Poweroff) Run(r *pipeline.Run) error { return poweroff(r) }

func poweroff(r *pipeline.Run) error {
	r.Printf("Powering %s off from inside", r.Domain)
	_ = r.Shell.Run(r.Ctx, r.Addr, rootPoweroff, guest.Line(r.Secret(pipeline.GuestPassword)), io.Discard, io.Discard)
	var stateErr error
	err := r.Poll(shutdownTimeout, pollInterval, r.Domain+" did not power off", func() bool {
		var off bool
		off, stateErr = shutOff(r)
		return off || stateErr != nil
	})
	if stateErr != nil {
		return stateErr
	}
	if err != nil {
		return err
	}
	r.Printf("%s is shut off", r.Domain)
	return nil
}

var rootPoweroff = guest.RootCommand("systemctl poweroff --check-inhibitors=no")

// Reboot powers the guest off from inside, boots it and waits for SSH.
type Reboot struct{}

func (Reboot) Check(r *pipeline.Run) error {
	declarePassword(r)
	declareDiskPassword(r)
	return nil
}

func (Reboot) Run(r *pipeline.Run) error {
	if err := poweroff(r); err != nil {
		return err
	}
	if err := boot(r); err != nil {
		return err
	}
	return waitSSH(r)
}

// sshReply tries one login at addr through shell. answered is set when sshd
// answered, accepted when it let the login in.
func sshReply(r *pipeline.Run, shell guest.Shell, addr string) (accepted, answered bool) {
	var stderr bytes.Buffer
	a := r.Addr
	a.Host = addr
	err := shell.Run(r.Ctx, a, "true", nil, io.Discard, &stderr)
	if err == nil {
		return true, true
	}
	return false, strings.Contains(stderr.String(), "Permission denied")
}
