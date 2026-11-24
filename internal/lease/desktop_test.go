package lease_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// desktopShell is a guest whose desktop session comes up when session is
// set. With luks it answers SSH only once keys were typed. It keeps each
// command and its stdin, and the keyring helper names the login keyring.
type desktopShell struct {
	f       *virttest.Fake
	luks    bool
	session bool
	cmds    []string
	stdin   map[string]string
}

func (s *desktopShell) Run(_ context.Context, _ guest.Addr, cmd string, in io.Reader, stdout, _ io.Writer) error {
	if s.luks && !slices.ContainsFunc(s.f.Calls(), func(c string) bool { return strings.HasPrefix(c, "SendKey ") }) {
		return errors.New("connection refused")
	}
	s.cmds = append(s.cmds, cmd)
	if in != nil {
		data, _ := io.ReadAll(in)
		s.stdin[cmd] = string(data)
	}
	switch {
	case strings.HasPrefix(cmd, "pgrep") && !s.session:
		return errors.New("exit status 1")
	case strings.Contains(cmd, "unlock-keyring.js"):
		_, _ = io.WriteString(stdout, "/org/freedesktop/secrets/collection/login\n")
	}
	return nil
}

// ran is the first command that holds part, empty when none does.
func (s *desktopShell) ran(part string) string {
	i := slices.IndexFunc(s.cmds, func(c string) bool { return strings.Contains(c, part) })
	if i < 0 {
		return ""
	}
	return s.cmds[i]
}

func desktopEnv(t *testing.T, f *virttest.Fake, sh *desktopShell) lease.Env {
	t.Helper()
	sh.f, sh.stdin = f, map[string]string{}
	e := env(t, f, &shell{})
	e.Shell = func(string) guest.Shell { return sh }
	e.H = screen{f}
	e.GuestDir = "/tmp/werft-provision"
	return e
}

var guestPassword = secret.Spec{Keyring: map[string]string{"service": "werft", "username": "tester"}}

// TestStartDesktopLogsInOnUbuntu: --desktop turns on GDM automatic login,
// waits for gnome-shell of the user, and unlocks the keyring with the guest
// password, which shows in no output.
func TestStartDesktopLogsInOnUbuntu(t *testing.T) {
	f := world(t)
	sh := &desktopShell{session: true}
	e := desktopEnv(t, f, sh)
	var out bytes.Buffer
	e.Out = &out
	e.Secrets, e.Terminal = store{"service=werft username=tester": "gpw"}, noTerminal{}
	l, err := lease.Start(e, record(), lease.Options{Owner: "agent", Desktop: true, GuestPassword: guestPassword})
	must(t, err)

	autologin := sh.ran("AutomaticLoginEnable=true")
	if !strings.HasPrefix(autologin, "sudo ") || !strings.Contains(autologin, "systemctl restart gdm") {
		t.Errorf("autologin command = %q, want the GDM file written and GDM restarted as root", autologin)
	}
	if sh.ran("pgrep") == "" || !strings.Contains(sh.ran("pgrep"), "gnome-shell") {
		t.Errorf("commands = %q, want a wait for gnome-shell", sh.cmds)
	}
	unlock := sh.ran("unlock-keyring.js")
	if !strings.Contains(unlock, "/tmp/werft-provision/unlock-keyring.js") || sh.stdin[unlock] != "gpw\n" {
		t.Errorf("unlock = %q with stdin %q, want the helper with the guest password", unlock, sh.stdin[unlock])
	}
	doc, err := json.Marshal(l)
	must(t, err)
	if strings.Contains(string(doc), "gpw") || strings.Contains(out.String(), "gpw") {
		t.Errorf("the guest password shows in the JSON or the output:\n%s\n%s", doc, out.String())
	}
	for _, c := range sh.cmds {
		if strings.Contains(c, "gpw") {
			t.Errorf("the guest password is on a command line: %q", c)
		}
	}
}

// TestStartDesktopOnSystemd255: at systemd 255 the lease reaches the guest
// at its DHCP address, logs in to the desktop as on a later systemd, and
// warns that the guest password goes to a TCP peer with an unchecked host
// key.
func TestStartDesktopOnSystemd255(t *testing.T) {
	f := world(t)
	sh := &desktopShell{session: true}
	e := desktopEnv(t, f, sh)
	var out bytes.Buffer
	e.Out = &out
	e.Secrets, e.Terminal = store{"service=werft username=tester": "gpw"}, noTerminal{}
	added := false
	e.Sleep = func(time.Duration) {
		if !added {
			added = true
			names, _ := f.Domains()
			text, _ := f.DomainXML(names[0].Name, true)
			d, _ := domxml.Parse(text)
			f.AddDHCPLease("default", virt.DHCPLease{MAC: domxml.Interfaces(d)[0].MAC, IP: "192.168.122.40"})
		}
	}
	rec := record()
	rec.Systemd = 255
	l, err := lease.Start(e, rec, lease.Options{Owner: "agent", Desktop: true, GuestPassword: guestPassword})
	must(t, err)
	if l.Address != "192.168.122.40" {
		t.Errorf("address = %s, want the DHCP address", l.Address)
	}
	if !strings.Contains(out.String(), "warning:") || !strings.Contains(out.String(), "host key") {
		t.Errorf("output = %q, want a warning about the unchecked host key", out.String())
	}
	unlock := sh.ran("unlock-keyring.js")
	if sh.ran("AutomaticLoginEnable=true") == "" || sh.stdin[unlock] != "gpw\n" {
		t.Errorf("commands = %q, want autologin and the keyring unlocked with the guest password", sh.cmds)
	}
}

// TestStartDesktopOnOmarchy: Omarchy logs in by itself, so werft writes no
// GDM file; it waits for Hyprland and unlocks the keyring with the disk
// password it typed, which is the guest password, read once.
func TestStartDesktopOnOmarchy(t *testing.T) {
	f := world(t)
	sh := &desktopShell{luks: true, session: true}
	e := desktopEnv(t, f, sh)
	reads := 0
	e.Secrets, e.Terminal = counting{store{"werft/guest-password": "pw"}, &reads}, noTerminal{}
	rec := luksRecord()
	rec.Family = "omarchy"
	if _, err := lease.Start(e, rec, lease.Options{Owner: "agent", Desktop: true, GuestPassword: guestPassword}); err != nil {
		t.Fatal(err)
	}
	if c := sh.ran("AutomaticLoginEnable"); c != "" {
		t.Errorf("an Omarchy lease wrote the GDM file: %q", c)
	}
	if !strings.Contains(sh.ran("pgrep"), "Hyprland") {
		t.Errorf("commands = %q, want a wait for Hyprland", sh.cmds)
	}
	if unlock := sh.ran("unlock-keyring.js"); sh.stdin[unlock] != "pw\n" {
		t.Errorf("unlock stdin = %q, want the disk password", sh.stdin[unlock])
	}
	if reads != 1 {
		t.Errorf("the lease read %d secrets, want the disk password once", reads)
	}
}

// counting is a store that counts its reads.
type counting struct {
	store
	n *int
}

func (c counting) Show(entry string) (string, error) {
	*c.n++
	return c.store.Show(entry)
}

func (c counting) Lookup(attributes map[string]string) (string, error) {
	*c.n++
	return c.store.Lookup(attributes)
}

// TestStartDesktopWithoutTheGuestPassword: a desktop lease that cannot read
// the guest password fails with secret_unavailable before it calls libvirt,
// and says what to do. A lease without --desktop reads no password.
func TestStartDesktopWithoutTheGuestPassword(t *testing.T) {
	tests := map[string]struct {
		spec secret.Spec
		want []string
	}{
		"the keyring is locked": {guestPassword, []string{"ask the user to unlock the login keyring", "do not print or ask for the password yourself"}},
		"no recipe names it":    {secret.Spec{}, []string{"guest_password.keyring or guest_password.pass", "config.toml gives vm"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := world(t)
			e := env(t, f, &shell{up: true})
			e.Secrets, e.Terminal = store{}, noTerminal{}
			before := len(f.Calls())
			_, err := lease.Start(e, record(), lease.Options{Owner: "agent", Desktop: true, GuestPassword: tt.spec})
			if !failure.Is(err, failure.SecretUnavailable) {
				t.Fatalf("Start = %v, want secret_unavailable", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error = %q, want it to say %q", err, w)
				}
			}
			for _, c := range f.Calls()[before:] {
				t.Errorf("Start without the guest password called libvirt: %s", c)
			}
			if _, err := os.Stat(e.Dir.Leases()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Start without the guest password made %s: %v", e.Dir.Leases(), err)
			}
		})
	}
	f := world(t)
	e := env(t, f, &shell{up: true})
	e.Secrets, e.Terminal = store{}, noTerminal{}
	if _, err := lease.Start(e, record(), lease.Options{Owner: "agent", GuestPassword: guestPassword}); err != nil {
		t.Errorf("control: a lease without --desktop = %v, want no password read", err)
	}
}

// TestStartDesktopOfAnUnknownFamily: a family werft cannot log in to fails
// with unsupported before any secret or libvirt call.
func TestStartDesktopOfAnUnknownFamily(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	reads := 0
	e.Secrets, e.Terminal = counting{store{}, &reads}, noTerminal{}
	rec := record()
	rec.Family = ""
	before := len(f.Calls())
	if _, err := lease.Start(e, rec, lease.Options{Owner: "agent", Desktop: true, GuestPassword: guestPassword}); !failure.Is(err, failure.Unsupported) {
		t.Fatalf("Start = %v, want unsupported", err)
	}
	if reads != 0 || len(f.Calls()) != before {
		t.Errorf("Start read %d secrets and called libvirt %v", reads, f.Calls()[before:])
	}
}

// TestStartDesktopWithoutASessionReleasesTheLease: a session that does not
// come up fails the lease with a timeout, and the lease is released.
func TestStartDesktopWithoutASessionReleasesTheLease(t *testing.T) {
	f := world(t)
	sh := &desktopShell{}
	e := desktopEnv(t, f, sh)
	e.Secrets, e.Terminal = store{"service=werft username=tester": "gpw"}, noTerminal{}
	l, err := lease.Start(e, record(), lease.Options{Owner: "agent", Desktop: true, GuestPassword: guestPassword})
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "no desktop session") || !strings.Contains(err.Error(), "it is released") {
		t.Fatalf("Start = %v, want a released lease with a session timeout", err)
	}
	if _, derr := f.Domain(l.Domain); !failure.Is(derr, failure.NotFound) {
		t.Errorf("the lease domain %s stays: %v", l.Domain, derr)
	}
	if c := sh.ran("unlock-keyring.js"); c != "" {
		t.Errorf("the lease ran the keyring helper without a session: %q", c)
	}
}
