package steps

import (
	"fmt"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/hostdev"
	"github.com/hk9890/werft/internal/pipeline"
)

// Viewer opens one viewer window to watch the run; it follows the VM across
// its reboots. The cleanup closes it: with nothing left to watch it would
// wait for a restart that never comes. A failed run leaves it open on the VM
// it leaves running for inspection. Over SSH to the host there is no
// display, and the window would fail without a word.
type Viewer struct{}

func (Viewer) KeepOnFailure() {}

func (Viewer) Check(r *pipeline.Run) error {
	if !r.Host.HasDisplay() {
		r.Printf("No display here, so no window opens. To watch the run, open one on the desktop: %s", ViewerCommand(r.Domain))
	}
	return nil
}

func (Viewer) Run(r *pipeline.Run) error {
	if !r.Host.HasDisplay() {
		return nil
	}
	stop, err := r.Host.OpenViewer(r.Domain, true)
	if err != nil {
		r.Printf("warning: no viewer window: %v", err)
		return nil
	}
	r.SetViewer(stop)
	r.Printf("Viewer window open: Shift+F11 fullscreen, Shift+F12 release cursor")
	return nil
}

func (Viewer) Cleanup(r *pipeline.Run) error {
	r.CloseViewer()
	return nil
}

// UsbPassthrough hands a host USB stick to the guest, live only: the device
// never enters the persistent XML, so neither a reset point nor the clone
// references it. The host lets go of the stick first; the cleanup takes it
// back. The stick is the one mounted at Run.KeyMount, or with its label
// under the user's media directory.
type UsbPassthrough struct {
	Label string

	stick     hostdev.Stick
	mount     string
	domain    string
	unmounted bool
}

func (u *UsbPassthrough) Check(r *pipeline.Run) error {
	mount := r.KeyMount
	if mount == "" {
		mount = r.Host.StickMount(u.Label)
	}
	s, err := r.Host.FindStick(mount)
	if err != nil {
		return err
	}
	// A file manager window GNOME opens on a plugged-in stick keeps it busy,
	// and the unmount at the hand-over would fail after the reset.
	if holders := r.Host.StickHolders(s); holders != "" {
		return failure.New(failure.Conflict, "a program holds the stick at %s:\n%s\nclose it, leave the stick mounted, and run again; nothing changed", mount, holders)
	}
	u.stick = s
	u.mount = mount
	return nil
}

func (u *UsbPassthrough) Run(r *pipeline.Run) error {
	r.Printf("Handing the stick at %s to %s", u.mount, r.Domain)
	if err := r.Host.UnmountStick(u.stick); err != nil {
		return err
	}
	u.unmounted = true
	if err := r.H.AttachDevice(r.Domain, u.stick.HostdevXML()); err != nil {
		return err
	}
	u.domain = r.Domain
	probe := "findmnt -rn -S UUID=" + u.stick.UUID
	return r.Poll(mountTimeout, pollInterval, "the stick did not mount in "+r.Domain, func() bool { return r.Probe(probe) })
}

// Cleanup acts on each half only while it holds, so a failed attach still
// gets the stick mounted back on the host.
func (u *UsbPassthrough) Cleanup(r *pipeline.Run) error {
	var err error
	if u.domain != "" {
		_ = onGuest(r, u.domain, func() error {
			return r.Exec("udisksctl unmount -b /dev/disk/by-uuid/"+u.stick.UUID, nil)
		})
		if derr := r.H.DetachDevice(u.domain, u.stick.HostdevXML()); derr != nil {
			err = fmt.Errorf("could not detach the stick from %s: %w", u.domain, derr)
		}
		u.domain = ""
	}
	if u.unmounted {
		_ = r.Poll(stickBackTimeout, pollInterval/3, "the stick did not come back", func() bool { return r.Host.StickOnHost(u.stick) })
		if merr := r.Host.MountStick(u.stick); merr != nil {
			r.Printf("warning: could not mount the stick on the host again: %v", merr)
		}
		u.unmounted = false
		r.Printf("The stick is back on the host")
	}
	return err
}
