package steps

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/guest/assets"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/secret"
)

// sessionEnv lets an SSH command reach the bus of the desktop session.
const sessionEnv = `env XDG_RUNTIME_DIR=/run/user/$(id -u) DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus`

// VerifyGuestPassword checks the guest password with one sudo call right
// after SSH comes up, so a typo costs the reset and not half an hour.
type VerifyGuestPassword struct{}

func (VerifyGuestPassword) Check(r *pipeline.Run) error {
	declarePassword(r)
	return nil
}

func (VerifyGuestPassword) Run(r *pipeline.Run) error {
	if err := r.Root("true"); err != nil {
		return failure.New(failure.Failed, "sudo in %s did not accept the password", r.Domain)
	}
	return nil
}

// DetectGuest tells Omarchy (Arch, Hyprland, SDDM) from Ubuntu with GNOME.
type DetectGuest struct{}

func (DetectGuest) Check(*pipeline.Run) error { return nil }

func (DetectGuest) Run(r *pipeline.Run) error {
	r.Family, r.SessionProcess = "ubuntu", "gnome-shell"
	if r.Probe("[ -d /usr/share/omarchy ]") {
		r.Family, r.SessionProcess = "omarchy", "Hyprland"
	}
	r.Printf("%s is a %s guest", r.Domain, r.Family)
	return nil
}

// Hostname names the guest after its domain, so ssh <domain> resolves over
// mDNS.
type Hostname struct{}

func (Hostname) Check(r *pipeline.Run) error {
	declarePassword(r)
	return nil
}

func (Hostname) Run(r *pipeline.Run) error {
	r.Printf("Naming %s after its domain", r.Domain)
	if err := r.Root(assets.Hostname, r.Domain); err != nil {
		return failure.Wrap(failure.Failed, err, "could not set the hostname of %s", r.Domain)
	}
	return nil
}

// luksRootProbe succeeds when the root file system sits on a LUKS volume. -v
// drops the [/subvolume] findmnt appends on btrfs; lsblk -s lists the device
// and every one beneath it.
const luksRootProbe = `lsblk -sno TYPE "$(findmnt -nvo SOURCE /)" | grep -qx crypt`

// LUKSMark probes the root disk for LUKS and sets or clears the LUKS marker
// in the domain description; the rest of the description stays.
type LUKSMark struct{}

func (LUKSMark) Check(*pipeline.Run) error { return nil }

func (LUKSMark) Run(r *pipeline.Run) error {
	luks := r.Probe(luksRootProbe)
	desc, err := r.H.DomainDescription(r.Domain)
	if err != nil {
		return err
	}
	desc = strings.TrimRight(desc, "\n")
	var kept []string
	if desc != "" {
		for _, line := range strings.Split(desc, "\n") {
			if line != domxml.LUKSMarker {
				kept = append(kept, line)
			}
		}
	}
	if luks {
		r.Printf("The disk of %s asks for a password at boot; werft types the guest password there", r.Domain)
		kept = append(kept, domxml.LUKSMarker)
	}
	if next := strings.Join(kept, "\n"); next != desc {
		return r.H.SetDomainDescription(r.Domain, next)
	}
	return nil
}

const gdmConf = "/etc/gdm3/custom.conf"

// autologinOffProbe succeeds while automatic login is off, so the stock file
// with the key commented out passes. GDM reads 1 as true. The file must
// exist: the autologin step keeps it aside and puts it back.
const autologinOffProbe = `[ -f /etc/gdm3/custom.conf ] && ! grep -Eiq "^[[:space:]]*AutomaticLoginEnable[[:space:]]*=[[:space:]]*(true|1)[[:space:]]*$" /etc/gdm3/custom.conf`

const autologinScript = `set -e
cp -a "$2" "$2.werft"
printf "[daemon]\nAutomaticLoginEnable=true\nAutomaticLogin=%s\n" "$1" >"$2"
systemctl restart gdm`

const autologinRestore = `[ ! -e "$1.werft" ] || mv -f "$1.werft" "$1"`

// Autologin turns GDM automatic login on for the run, so nobody has to log
// in at the VM, and puts the guest's own file back in cleanup. GDM reads the
// file only at start; nobody is logged in yet, so the restart costs nothing.
// The cleanup restarts nothing, so a guest left running keeps its session.
// With Prepare it only checks that automatic login is off: the base stays as
// a person gets it, and the clone must ask for a password.
type Autologin struct {
	Prepare bool
	domain  string
}

func (a *Autologin) Check(r *pipeline.Run) error {
	if !a.Prepare {
		declarePassword(r)
	}
	return nil
}

func (a *Autologin) Run(r *pipeline.Run) error {
	if a.Prepare {
		if !r.Probe(autologinOffProbe) {
			return failure.New(failure.Conflict, "automatic login is on in %s, or %s is missing; the base must keep it off so %s asks for a password (provision turns it on for its own run). Turn it off there (Settings, System, Users) and run werft prepare %s again", r.Domain, gdmConf, r.Domain+resetpoint.FullSuffix, r.Domain)
		}
		return nil
	}
	r.Printf("Logging %s in to %s automatically for the run", r.User, r.Domain)
	a.domain = r.Domain
	if err := r.Root(autologinScript, r.User, gdmConf); err != nil {
		return failure.Wrap(failure.Failed, err, "could not turn on automatic login in %s", r.Domain)
	}
	return nil
}

func (a *Autologin) Cleanup(r *pipeline.Run) error {
	if a.domain == "" {
		return nil
	}
	err := onGuest(r, a.domain, func() error { return r.Root(autologinRestore, gdmConf) })
	if err != nil {
		return failure.Wrap(failure.Failed, err, "could not turn automatic login off again; restore %s from %s.werft in %s", gdmConf, gdmConf, a.domain)
	}
	a.domain = ""
	r.Printf("%s is back, so %s logs in with a password again", gdmConf, r.User)
	return nil
}

// onGuest runs fn with r reaching the running domain, whatever domain the
// run acts on now. A cleanup uses it: the guest may have rebooted since its
// step ran, and the run may have moved on to the clone.
func onGuest(r *pipeline.Run, domain string, fn func() error) error {
	saved, savedAddr := r.Domain, r.Addr
	defer func() { r.Domain, r.Addr = saved, savedAddr }()
	r.Domain = domain
	if !r.Running() {
		return fmt.Errorf("%s is not running", domain)
	}
	if !r.Reach("true") {
		return fmt.Errorf("%s does not answer on SSH", domain)
	}
	return fn()
}

// Upload empties the guest directory and copies the embedded helpers and
// the given host files into it. A boot empties it again.
type Upload struct{ Files []string }

func (Upload) Check(*pipeline.Run) error { return nil }

func (u Upload) Run(r *pipeline.Run) error {
	files := assets.Helpers()
	for _, f := range u.Files {
		files = append(files, guest.File{Name: path.Base(f), Source: f})
	}
	if err := guest.Copy(r.Ctx, r.Shell, r.Addr, r.GuestDir, files, r.Out); err != nil {
		return failure.Wrap(failure.Failed, err, "could not copy the files into %s", r.Domain)
	}
	return nil
}

// WaitSession waits for the desktop session of the user. The greeter's own
// gnome-shell runs as gdm, so only the user's counts.
type WaitSession struct{}

func (WaitSession) Check(*pipeline.Run) error { return nil }

func (WaitSession) Run(r *pipeline.Run) error {
	process := r.SessionProcess
	if process == "" {
		process = "gnome-shell"
	}
	probe := `pgrep -u "$(id -u)" -x ` + guest.Quote(process)
	if err := r.Poll(sessionTimeout, pollInterval, fmt.Sprintf("no desktop session of %s came up in %s", r.User, r.Domain), func() bool { return r.Probe(probe) }); err != nil {
		return err
	}
	r.Printf("Desktop session is up in %s", r.Domain)
	return nil
}

var keyringPath = regexp.MustCompile(`^/org/freedesktop/secrets/collection/[A-Za-z0-9_]+$`)

const secretServiceQuery = sessionEnv + " busctl --user get-property org.freedesktop.secrets /org/freedesktop/secrets org.freedesktop.Secret.Service Collections"

// UnlockKeyring unlocks the default keyring, or creates the login keyring on
// a base that has none, with the guest password: an automatic login leaves
// it locked, and gh keeps its token in it. It needs the helper upload copies.
type UnlockKeyring struct{}

func (UnlockKeyring) Check(r *pipeline.Run) error {
	declarePassword(r)
	return nil
}

func (UnlockKeyring) Run(r *pipeline.Run) error {
	if err := r.Poll(sessionTimeout, pollInterval, "the Secret Service did not answer in "+r.Domain, func() bool { return r.Probe(secretServiceQuery) }); err != nil {
		return err
	}
	r.Printf("Unlocking the default keyring in %s", r.Domain)
	keyring, err := r.Output(sessionEnv+" gjs "+guest.Quote(r.GuestDir+"/unlock-keyring.js"), guest.Line(r.Secret(pipeline.GuestPassword)))
	if err != nil {
		return failure.Wrap(failure.Failed, err, "the password did not unlock or create the default keyring in %s", r.Domain)
	}
	if !keyringPath.MatchString(keyring) {
		return failure.New(failure.Failed, "the keyring helper in %s named no keyring: %q", r.Domain, keyring)
	}
	if !r.Probe(sessionEnv + " busctl --user get-property org.freedesktop.secrets " + keyring + " org.freedesktop.Secret.Collection Locked | grep -qx 'b false'") {
		return failure.New(failure.Failed, "the default keyring in %s is still locked", r.Domain)
	}
	return nil
}

const guestSudoers = "/etc/sudoers.d/90-werft-provision"

// SudoGrant opens passwordless sudo for the guest user, for scripts that call
// sudo with no terminal to ask on, and closes it in cleanup. Neither the base
// nor the clone carries it.
type SudoGrant struct{ domain string }

func (s *SudoGrant) Check(r *pipeline.Run) error {
	declarePassword(r)
	return nil
}

func (s *SudoGrant) Run(r *pipeline.Run) error {
	r.Printf("Opening sudo in %s", r.Domain)
	// Set first: a grant that fails after the guest wrote the file is still
	// taken back.
	s.domain = r.Domain
	if err := r.Root(`umask 227; printf "%s ALL=(ALL) NOPASSWD:ALL\n" "$1" >"$2"`, r.User, guestSudoers); err != nil {
		return failure.Wrap(failure.Failed, err, "could not open sudo in %s", r.Domain)
	}
	return nil
}

func (s *SudoGrant) Cleanup(r *pipeline.Run) error {
	if s.domain == "" {
		return nil
	}
	if err := onGuest(r, s.domain, func() error { return r.Root(`rm -f "$1"`, guestSudoers) }); err != nil {
		return failure.Wrap(failure.Failed, err, "could not close sudo again; remove %s in %s", guestSudoers, s.domain)
	}
	s.domain = ""
	r.Printf("sudo asks for the password again")
	return nil
}

const pinentryScript = `set -e
dir="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
(umask 077; cat >"$dir/werft-passphrase")
conf="$HOME/.gnupg/gpg-agent.conf"
mkdir -p "$HOME/.gnupg"
chmod 700 "$HOME/.gnupg"
[ -e "$conf" ] || : >"$conf.werft-created"
if [ -s "$conf" ] && [ -n "$(tail -c 1 "$conf")" ]; then printf '\n' >>"$conf"; fi
printf 'pinentry-program %s/pinentry-auto\n' "$1" >>"$conf"
gpgconf --kill gpg-agent 2>/dev/null || true`

// pinentryRemove rewrites the file in place, so it keeps its owner and mode.
const pinentryRemove = `dir="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
rm -f "$dir/werft-passphrase"
conf="$HOME/.gnupg/gpg-agent.conf"
if [ -e "$conf.werft-created" ]; then
    rm -f "$conf" "$conf.werft-created"
elif [ -f "$conf" ]; then
    grep -vxF "pinentry-program $1/pinentry-auto" "$conf" >"$conf.werft-tmp" || true
    cat "$conf.werft-tmp" >"$conf"
    rm -f "$conf.werft-tmp"
fi
gpgconf --kill gpg-agent 2>/dev/null || true`

// Pinentry routes gpg-agent's passphrase prompts to the pinentry-auto stub,
// which answers with the secret. The secret lives in the user's runtime
// directory, a tmpfs, never on the disk. Cleanup removes both, so the image
// asks a person again.
type Pinentry struct {
	Secret string
	Spec   secret.Spec
	domain string
}

func (p *Pinentry) Check(r *pipeline.Run) error {
	r.Declare(p.Secret, p.Spec)
	return nil
}

func (p *Pinentry) Run(r *pipeline.Run) error {
	r.Printf("Routing GPG passphrase prompts in %s to pinentry-auto", r.Domain)
	p.domain = r.Domain
	return r.Exec(guest.ScriptCommand(pinentryScript, r.GuestDir), guest.Line(r.Secret(p.Secret)))
}

func (p *Pinentry) Cleanup(r *pipeline.Run) error {
	if p.domain == "" {
		return nil
	}
	if err := onGuest(r, p.domain, func() error { return r.Exec(guest.ScriptCommand(pinentryRemove, r.GuestDir), nil) }); err != nil {
		return failure.Wrap(failure.Failed, err, "could not remove pinentry-auto from ~/.gnupg/gpg-agent.conf in %s", p.domain)
	}
	p.domain = ""
	return nil
}

// Run runs a command in the guest directory, with a secret on stdin when one
// is named.
type Run struct {
	Command string
	Secret  string
	Spec    secret.Spec
}

func (s Run) Check(r *pipeline.Run) error {
	if s.Secret != "" {
		r.Declare(s.Secret, s.Spec)
	}
	return nil
}

func (s Run) Run(r *pipeline.Run) error {
	r.Printf("Running in %s: %s", r.Domain, s.Command)
	var stdin io.Reader
	if s.Secret != "" {
		stdin = guest.Line(r.Secret(s.Secret))
	}
	cmd := fmt.Sprintf("mkdir -p %s && cd %s && %s", guest.Quote(r.GuestDir), guest.Quote(r.GuestDir), s.Command)
	if err := r.Exec(cmd, stdin); err != nil {
		return failure.Wrap(failure.Failed, err, "the command failed in %s", r.Domain)
	}
	return nil
}

// Checks copies a host directory of checks into the guest directory, runs
// its entry script there and fails with checks_failed when it fails. The
// checks come from the host, so a check added on a branch runs at once.
type Checks struct {
	Dir     string
	Command string
}

func (Checks) Check(*pipeline.Run) error { return nil }

func (c Checks) Run(r *pipeline.Run) error {
	entry := c.Command
	if entry == "" {
		entry = "run.sh"
	}
	dir := r.GuestDir + "/" + path.Base(c.Dir)
	if err := guest.Copy(r.Ctx, r.Shell, r.Addr, dir, []guest.File{{Name: ".", Source: c.Dir}}, r.Out); err != nil {
		return failure.Wrap(failure.Failed, err, "could not copy the checks into %s", r.Domain)
	}
	r.Printf("Running the checks of %s in %s", c.Dir, r.Domain)
	if err := r.Exec("cd "+guest.Quote(dir)+" && bash "+guest.Quote(entry), nil); err != nil {
		r.Printf("checks: FAIL")
		return failure.Wrap(failure.ChecksFailed, err, "the checks failed in %s", r.Domain)
	}
	r.Printf("checks: PASS")
	return nil
}
