package steps

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/hostdev"
)

// System is the pipeline.Host of this machine.
type System struct {
	User string
	// Out gets the output of the host tools.
	Out io.Writer
}

func (System) HasDisplay() bool {
	return os.Getenv("DISPLAY")+os.Getenv("WAYLAND_DISPLAY") != ""
}

// ViewerCommand is a command a person runs on the desktop to watch domain.
// It changes nothing: it waits for the domain to start and follows it
// across reboots.
func ViewerCommand(domain string) string {
	return "virt-viewer -c qemu:///system --wait --reconnect " + domain
}

// OpenViewer starts virt-viewer in a session of its own, so it outlives
// werft unless stop closes it. The hotkeys are set because virt-manager's
// console defines no fullscreen key, which is how a guest ends up holding
// the screen with no way back.
func (System) OpenViewer(domain string, reconnect bool) (func(), error) {
	args := []string{"-c", "qemu:///system", "--attach", "--wait",
		"--hotkeys=toggle-fullscreen=shift+f11,release-cursor=shift+f12"}
	if reconnect {
		args = append(args, "--reconnect")
	}
	c := exec.Command("virt-viewer", append(args, "--", domain)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return nil, err
	}
	go func() { _ = c.Wait() }()
	return func() { _ = c.Process.Kill() }, nil
}

const virtManagerVMs = "/org/virt-manager/virt-manager/vms/"

// CopyViewerSettings copies virt-manager's settings of one domain to
// another. A host without dconf has none to copy.
func (System) CopyViewerSettings(fromUUID, toUUID string) error {
	if _, err := exec.LookPath("dconf"); err != nil {
		return nil
	}
	dump, err := exec.Command("dconf", "dump", virtManagerVMs+strings.ReplaceAll(fromUUID, "-", "")+"/").Output()
	if err != nil {
		return err
	}
	load := exec.Command("dconf", "load", virtManagerVMs+strings.ReplaceAll(toUUID, "-", "")+"/")
	load.Stdin = bytes.NewReader(dump)
	return load.Run()
}

func (s System) StickMount(label string) string { return hostdev.MountOf(s.User, label) }

func (System) FindStick(mount string) (hostdev.Stick, error) { return hostdev.Find(mount) }

func (System) StickHolders(st hostdev.Stick) string { return hostdev.Holders(st) }

func (System) UnmountStick(st hostdev.Stick) error { return hostdev.Unmount(st) }

func (System) MountStick(st hostdev.Stick) error { return hostdev.Mount(st) }

func (System) StickOnHost(st hostdev.Stick) bool { return hostdev.OnHost(st) }

// CopySSHKey runs ssh-copy-id. -f skips its own trial login, one more failed
// login for sshd to count. ssh asks for the password on the terminal.
func (s System) CopySSHKey(addr guest.Addr, pubkey string) error {
	args := []string{"-f", "-i", pubkey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"}
	if strings.HasPrefix(addr.Host, "vsock/") {
		args = append(args, "-o", "ProxyCommand="+guest.SSHProxy+" %h %p", "-o", "ProxyUseFdpass=yes")
	}
	c := exec.Command("ssh-copy-id", append(args, addr.String())...)
	c.Stdout, c.Stderr = s.Out, s.Out
	return c.Run()
}
