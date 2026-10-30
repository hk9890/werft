// Package steps holds the built-in steps of prepare and provision
// (docs/DESIGN.md, "Built-in steps"), and the fixed step list of prepare.
package steps

import (
	"bytes"
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
	if !shutOff(r) {
		r.Printf("Forcing %s off", r.Domain)
		if err := r.H.DestroyDomain(r.Domain); err != nil {
			return err
		}
	}
	r.Printf("Resetting %s to '%s'", r.Domain, s.label())
	return resetpoint.Revert(r.H, r.Domain, s.label())
}

func shutOff(r *pipeline.Run) bool {
	s, err := r.H.DomainState(r.Domain)
	return err == nil && s == virt.StateShutOff
}

// Boot starts the domain. An encrypted guest takes no address before its
// disk is open, so the password is typed first, once: keys sent before the
// prompt are lost, and cryptsetup allows few tries.
type Boot struct{}

func (Boot) Check(r *pipeline.Run) error {
	declareDiskPassword(r)
	return nil
}

func (Boot) Run(r *pipeline.Run) error { return boot(r) }

func boot(r *pipeline.Run) error {
	if !r.Running() {
		if err := r.H.StartDomain(r.Domain); err != nil {
			return err
		}
		r.Printf("%s started (headless, no window)", r.Domain)
	}
	r.LUKS = hasLUKSMarker(r, r.Domain)
	if !r.LUKS {
		return nil
	}
	r.Printf("Waiting for the disk password prompt of %s", r.Domain)
	p := console.DefaultPrompt
	p.Sleep = r.Sleep
	if err := console.WaitForPrompt(r.Ctx, r.H, r.Domain, p); err != nil {
		return failure.Wrap(failure.KindOf(err), err, "open the window to look: virt-viewer -c qemu:///system %s", r.Domain)
	}
	r.Printf("Typing the disk password of %s at its console", r.Domain)
	return console.Type(r.H, r.Domain, r.Secret(pipeline.GuestPassword))
}

// WaitSSH waits until sshd in the guest answers and sets the address.
type WaitSSH struct{}

func (WaitSSH) Check(*pipeline.Run) error { return nil }

func (WaitSSH) Run(r *pipeline.Run) error { return waitSSH(r) }

func waitSSH(r *pipeline.Run) error {
	err := r.PollLogin(sshTimeout, "no SSH answer from "+r.Domain, func() bool { return r.Reach("true") })
	if err != nil && r.LUKS {
		return failure.Wrap(failure.KindOf(err), err, "if its console asks for the disk password again, the disk refused it; nothing types it twice (virt-viewer -c qemu:///system %s)", r.Domain)
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
	if err := r.Poll(shutdownTimeout, pollInterval, r.Domain+" did not power off", func() bool { return shutOff(r) }); err != nil {
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

func sshReply(r *pipeline.Run, addr string) (accepted, answered bool) {
	var stderr bytes.Buffer
	a := r.Addr
	a.Host = addr
	err := r.KeyShell.Run(r.Ctx, a, "true", nil, io.Discard, &stderr)
	if err == nil {
		return true, true
	}
	return false, strings.Contains(stderr.String(), "Permission denied")
}
