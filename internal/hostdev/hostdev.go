// Package hostdev is the host side of USB passthrough: it finds a mounted
// USB stick, makes sure no host process holds it, and lets go of it and takes
// it back with udisks, which needs no sudo.
package hostdev

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hk9890/werft/internal/failure"
)

// Stick is a USB stick. UUID, the file system UUID, addresses it from host
// and guest: unlike a label it never holds a space.
type Stick struct {
	Device  string
	UUID    string
	Vendor  string
	Product string
}

// HostdevXML is the libvirt device that hands the stick to a domain.
func (s Stick) HostdevXML() string {
	return fmt.Sprintf("<hostdev mode='subsystem' type='usb' managed='yes'><source><vendor id='0x%s'/><product id='0x%s'/></source></hostdev>", s.Vendor, s.Product)
}

// MountOf is where udisks mounts a stick labelled label: under /media on
// Ubuntu, under /run/media elsewhere. It is the /media path when neither is
// mounted.
func MountOf(user, label string) string {
	media := filepath.Join("/media", user, label)
	if !mounted(media) && mounted("/run"+media) {
		return "/run" + media
	}
	return media
}

func mounted(path string) bool {
	return exec.Command("findmnt", path).Run() == nil
}

var uuidPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Find is the USB stick mounted at mount. udev reports the bus and ids on the
// mounted node itself, partition or whole disk.
func Find(mount string) (Stick, error) {
	out, err := exec.Command("findmnt", "-no", "SOURCE", "--", mount).Output()
	if err != nil {
		return Stick{}, failure.New(failure.NotFound, "nothing is mounted at %s; plug in the stick", mount)
	}
	s := Stick{Device: strings.TrimSpace(string(out))}
	props, err := exec.Command("udevadm", "info", "-q", "property", "-n", s.Device).Output()
	if err != nil {
		return Stick{}, failure.Wrap(failure.NotFound, err, "udev knows nothing about %s", s.Device)
	}
	p := map[string]string{}
	for _, line := range strings.Split(string(props), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			p[k] = v
		}
	}
	if p["ID_BUS"] != "usb" {
		return Stick{}, failure.New(failure.Usage, "%s is not on a USB device", mount)
	}
	s.Vendor, s.Product, s.UUID = p["ID_VENDOR_ID"], p["ID_MODEL_ID"], p["ID_FS_UUID"]
	if s.Vendor == "" || s.Product == "" || !uuidPattern.MatchString(s.UUID) {
		return Stick{}, failure.New(failure.Failed, "could not identify the USB stick behind %s", s.Device)
	}
	return s, nil
}

// Holders is the fuser report of the processes that hold the stick, empty
// when none does. The kernel's own hold on the mount is no process.
func Holders(s Stick) string {
	pids, _ := exec.Command("fuser", "-m", s.Device).Output()
	if len(bytes.TrimSpace(pids)) == 0 {
		return ""
	}
	report, _ := exec.Command("fuser", "-vm", s.Device).CombinedOutput()
	return string(report)
}

func Unmount(s Stick) error {
	if out, err := exec.Command("udisksctl", "unmount", "-b", s.Device).CombinedOutput(); err != nil {
		return failure.Wrap(failure.Conflict, err, "could not unmount %s on the host; close what holds it and run again: %s\n%s", s.Device, bytes.TrimSpace(out), Holders(s))
	}
	return nil
}

// OnHost reports whether the host sees the stick's file system.
func OnHost(s Stick) bool {
	_, err := os.Stat("/dev/disk/by-uuid/" + s.UUID)
	return err == nil
}

func Mount(s Stick) error {
	if out, err := exec.Command("udisksctl", "mount", "-b", "/dev/disk/by-uuid/"+s.UUID).CombinedOutput(); err != nil {
		return fmt.Errorf("udisksctl mount: %s: %w", bytes.TrimSpace(out), err)
	}
	return nil
}
