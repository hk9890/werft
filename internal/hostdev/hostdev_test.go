package hostdev_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/hostdev"
)

// stubs puts findmnt, udevadm, fuser and udisksctl on PATH, as vm_test.sh
// does. The stick is mounted at $STICK_MOUNT from /dev/sdz1, or from the
// whole disk /dev/sdz with STICK_WHOLE_DISK=1. Each call goes to the log.
func stubs(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	t.Setenv("STUB_LOG", log)
	for name, body := range map[string]string{
		"findmnt": `for last; do :; done
[ "$last" = "$STICK_MOUNT" ] || exit 1
if [ "$STICK_WHOLE_DISK" = 1 ]; then echo /dev/sdz; else echo /dev/sdz1; fi`,
		"udevadm": `if [ "$STICK_NOT_USB" = 1 ]; then echo ID_BUS=ata; else echo ID_BUS=usb; fi
echo ID_VENDOR_ID=0781
echo ID_MODEL_ID=5581
echo "ID_FS_UUID=${STICK_UUID:-ABCD-1234}"`,
		// fuser prints the process ids on stdout and its table on stderr.
		"fuser": `[ -n "$STICK_BUSY" ] || exit 1
[ "$1" = -vm ] && printf 'USER PID ACCESS COMMAND\n/dev/sdz1: user 4242 f.... nautilus\n' >&2
printf ' 4242'`,
		"udisksctl": `[ "$1" != unmount ] || [ "$STICK_UNMOUNT_FAIL" != 1 ] || { echo "target is busy" >&2; exit 1; }`,
	} {
		script := "#!/bin/sh\necho \"" + name + " $*\" >>\"$STUB_LOG\"\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"STICK_WHOLE_DISK", "STICK_NOT_USB", "STICK_BUSY", "STICK_UNMOUNT_FAIL", "STICK_UUID"} {
		t.Setenv(k, "")
	}
	t.Setenv("STICK_MOUNT", "/media/tester/KEYSTICK")
	return log
}

func calls(log string) string {
	data, _ := os.ReadFile(log)
	return string(data)
}

func TestMountOfFindsTheLabelUnderMediaOrRunMedia(t *testing.T) {
	stubs(t)
	if m := hostdev.MountOf("tester", "KEYSTICK"); m != "/media/tester/KEYSTICK" {
		t.Errorf("MountOf = %s", m)
	}
	t.Setenv("STICK_MOUNT", "/run/media/tester/KEYSTICK")
	if m := hostdev.MountOf("tester", "KEYSTICK"); m != "/run/media/tester/KEYSTICK" {
		t.Errorf("MountOf on Omarchy = %s", m)
	}
	t.Setenv("STICK_MOUNT", "/elsewhere")
	if m := hostdev.MountOf("tester", "KEYSTICK"); m != "/media/tester/KEYSTICK" {
		t.Errorf("MountOf without a stick = %s, want the default to name", m)
	}
}

func TestFind(t *testing.T) {
	stubs(t)
	s, err := hostdev.Find("/media/tester/KEYSTICK")
	if err != nil {
		t.Fatal(err)
	}
	want := hostdev.Stick{Device: "/dev/sdz1", UUID: "ABCD-1234", Vendor: "0781", Product: "5581"}
	if s != want {
		t.Errorf("Find = %+v", s)
	}
	if x := s.HostdevXML(); x != "<hostdev mode='subsystem' type='usb' managed='yes'><source><vendor id='0x0781'/><product id='0x5581'/></source></hostdev>" {
		t.Errorf("HostdevXML = %s", x)
	}

	t.Setenv("STICK_WHOLE_DISK", "1")
	if s, err := hostdev.Find("/media/tester/KEYSTICK"); err != nil || s.Device != "/dev/sdz" {
		t.Errorf("a stick without a partition table: %+v, %v", s, err)
	}
}

func TestFindRefusals(t *testing.T) {
	tests := []struct {
		name, mount, env, value string
		kind                    failure.Kind
		message                 string
	}{
		{"nothing mounted", "/media/tester/NONE", "", "", failure.NotFound, "nothing is mounted at /media/tester/NONE; plug in the stick"},
		{"not USB", "/media/tester/KEYSTICK", "STICK_NOT_USB", "1", failure.Usage, "/media/tester/KEYSTICK is not on a USB device"},
		// The UUID goes into a guest command line.
		{"odd UUID", "/media/tester/KEYSTICK", "STICK_UUID", "AB CD;x", failure.Failed, "could not identify the USB stick behind /dev/sdz1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubs(t)
			if tt.env != "" {
				t.Setenv(tt.env, tt.value)
			}
			_, err := hostdev.Find(tt.mount)
			if !failure.Is(err, tt.kind) || err.Error() != tt.message {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestHolders(t *testing.T) {
	log := stubs(t)
	s := hostdev.Stick{Device: "/dev/sdz1", UUID: "ABCD-1234"}
	if h := hostdev.Holders(s); h != "" {
		t.Errorf("a free stick has holders %q", h)
	}
	t.Setenv("STICK_BUSY", "1")
	if h := hostdev.Holders(s); !strings.Contains(h, "nautilus") {
		t.Errorf("Holders = %q, want the fuser table", h)
	}
	if c := calls(log); !strings.Contains(c, "fuser -m /dev/sdz1\n") {
		t.Errorf("calls:\n%s", c)
	}
}

func TestUnmountAndMount(t *testing.T) {
	log := stubs(t)
	s := hostdev.Stick{Device: "/dev/sdz1", UUID: "ABCD-1234"}
	if err := hostdev.Unmount(s); err != nil {
		t.Fatal(err)
	}
	if err := hostdev.Mount(s); err != nil {
		t.Fatal(err)
	}
	if c := calls(log); c != "udisksctl unmount -b /dev/sdz1\nudisksctl mount -b /dev/disk/by-uuid/ABCD-1234\n" {
		t.Errorf("calls:\n%s", c)
	}

	t.Setenv("STICK_UNMOUNT_FAIL", "1")
	err := hostdev.Unmount(s)
	if !failure.Is(err, failure.Conflict) || !strings.Contains(err.Error(), "target is busy") {
		t.Errorf("err = %v, want the udisksctl reason", err)
	}
	if msg := err.Error(); strings.Contains(msg, "close what holds it") || strings.HasSuffix(msg, "\n") {
		t.Errorf("err = %q, want no holder advice and no trailing newline without holders", msg)
	}

	t.Setenv("STICK_BUSY", "1")
	err = hostdev.Unmount(s)
	if msg := err.Error(); !strings.Contains(msg, "close what holds it and run again:\n") || !strings.Contains(msg, "nautilus") || strings.HasSuffix(msg, "\n") {
		t.Errorf("err = %q, want the holders named", msg)
	}
}
