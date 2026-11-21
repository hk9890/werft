package steps_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/hostdev"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/recipe"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// keyEnter is the Linux key code of Enter.
const keyEnter = 28

const (
	werftPubKey = "ssh-ed25519 AAAAwerft werft"
	userPubKey  = "ssh-ed25519 AAAAuser tester@host"
	vm          = "testvm"
	password    = "gu3st pw"
	passphrase  = "s3cr%t"
)

// world is a build VM on the fake hypervisor with a fake guest, host,
// terminal and pass. Every action that matters lands in one log, in order,
// as in vm_test.sh.
type world struct {
	t     *testing.T
	fake  *virttest.Fake
	h     *recorder
	shell *shell
	host  *host
	term  *terminal
	pass  map[string]string
	out   bytes.Buffer
	// screen is the console of every domain, the disk prompt of Omarchy.
	screen []byte
	// key is the werft key, userKey the user's own; only their public
	// halves exist.
	key     string
	userKey string

	mu  sync.Mutex
	log []string
}

func (w *world) add(entry string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, entry)
}

func (w *world) entries() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.log...)
}

const domainXML = `<domain type='kvm'>
  <name>%s</name>
  <description>my notes</description>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <loader readonly='yes' type='pflash' format='raw'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
    <nvram format='qcow2'>/var/lib/libvirt/qemu/nvram/%s_VARS.qcow2</nvram>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/isos/ubuntu.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <interface type='network'><mac address='52:54:00:00:00:01'/><source network='default'/></interface>
  </devices>
</domain>`

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{t: t, fake: virttest.New(), pass: map[string]string{},
		key: filepath.Join(dir, "werft", "id_ed25519"), userKey: filepath.Join(dir, "user", "id_ed25519")}
	must(t, os.MkdirAll(filepath.Dir(w.key), 0o700))
	must(t, os.MkdirAll(filepath.Dir(w.userKey), 0o700))
	must(t, os.WriteFile(w.key+".pub", []byte(werftPubKey+"\n"), 0o644))
	must(t, os.WriteFile(w.userKey+".pub", []byte(userPubKey+"\n"), 0o644))
	w.h = &recorder{Fake: w.fake, w: w}
	w.shell = &shell{w: w, systemd: 259, fail: map[string]bool{}, drop: map[string]bool{}}
	w.host = &host{w: w, display: true}
	w.term = &terminal{w: w, tty: true, answer: true, secrets: map[string]string{
		"GPG passphrase for the key on KEYSTICK":            passphrase,
		"Password of tester in the VM testvm, for its sudo": password,
	}}
	must(t, w.fake.DefinePool("default", "/var/lib/libvirt/images"))
	must(t, w.fake.StartPool("default"))
	must(t, w.fake.DefinePool("nvram", "/var/lib/libvirt/qemu/nvram"))
	must(t, w.fake.StartPool("nvram"))
	_, err := w.fake.CreateVolume("nvram", "<volume><name>testvm_VARS.qcow2</name><capacity>540672</capacity><target><format type='qcow2'/></target></volume>")
	must(t, err)
	disk, err := w.fake.CreateVolume("default", "<volume><name>testvm.qcow2</name><capacity unit='G'>20</capacity><target><format type='qcow2'/></target></volume>")
	must(t, err)
	_, err = w.fake.DefineDomain(fmt.Sprintf(domainXML, vm, vm, disk.Path))
	must(t, err)
	must(t, w.fake.CreateSnapshot(vm, resetpoint.ProvisionBase, ""))
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// defineClone defines a shut-off testvm-full with a disk and an NVRAM of
// its own.
func (w *world) defineClone() {
	disk, err := w.fake.CreateVolume("default", "<volume><name>testvm-full.qcow2</name><capacity unit='G'>20</capacity><target><format type='qcow2'/></target></volume>")
	must(w.t, err)
	_, err = w.fake.CreateVolume("nvram", "<volume><name>testvm-full_VARS.qcow2</name><capacity>540672</capacity><target><format type='qcow2'/></target></volume>")
	must(w.t, err)
	xml := fmt.Sprintf(domainXML, "testvm-full", "testvm-full", disk.Path)
	_, err = w.fake.DefineDomain(strings.Replace(xml, "52:54:00:00:00:01", "52:54:00:00:00:02", 1))
	must(w.t, err)
}

// omarchy makes the guest Omarchy, with its root on LUKS and the marker vm
// prepare sets, and a console that shows the disk prompt. Each start locks
// the disk until Enter at the console.
func (w *world) omarchy() {
	w.shell.omarchy, w.shell.luks, w.shell.locks = true, true, true
	must(w.t, w.fake.SetDomainDescription(vm, "my notes\n"+domxml.LUKSMarker))
	must(w.t, w.fake.DeleteSnapshot(vm, resetpoint.ProvisionBase))
	must(w.t, w.fake.CreateSnapshot(vm, resetpoint.ProvisionBase, ""))
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.White)
	var buf bytes.Buffer
	must(w.t, png.Encode(&buf, img))
	w.screen = buf.Bytes()
	w.fake.SetScreenshot(vm, "image/png", w.screen)
}

func (w *world) run(entries []pipeline.Entry, yes bool) (*pipeline.Run, error) {
	r := w.newRun(yes)
	return r, pipeline.Execute(r, entries)
}

// runWithCancel runs with a context that *cancel cancels, as SIGINT does.
func (w *world) runWithCancel(entries []pipeline.Entry, cancel *context.CancelFunc) (*pipeline.Run, error) {
	r := w.newRun(true)
	r.Ctx, *cancel = context.WithCancel(context.Background())
	return r, pipeline.Execute(r, entries)
}

func (w *world) newRun(yes bool) *pipeline.Run {
	return (&pipeline.Run{
		H: w.h, Shell: w.shell, KeyShell: werftKeyShell{w.shell}, Host: w.host, Secrets: passStore{w}, Prompter: w.term,
		Out: &w.out, Yes: yes, Sleep: func(time.Duration) {},
		BuildVM: vm, Domain: vm, User: "tester", GuestDir: "/tmp/werft-provision",
		SSHConfig: "/state/ssh_config", SSHKey: w.key, UserKey: w.userKey, LogPath: "/state/logs/p.log",
		PasswordSpec: secret.Spec{Pass: "werft/guest-password", Prompt: "Password of tester in the VM testvm, for its sudo"},
		Addr:         guest.Addr{User: "tester"},
	}).Init()
}

// recorder logs the hypervisor calls that change something, and gives every
// MAC a DHCP lease, so a clone with a new MAC gets an address.
type recorder struct {
	*virttest.Fake
	w *world
	// noLeases gives no MAC an address, as a guest held in its initramfs.
	noLeases bool
	// starts renews every lease at each start, as a booting guest does.
	starts int64
	// staleLeases keeps the leases of an earlier boot and renews none, as a
	// guest held in its initramfs after a reboot.
	staleLeases bool
}

func (h *recorder) StartDomain(name string) error {
	h.w.add("StartDomain " + name)
	h.starts++
	h.w.shell.locked = h.w.shell.locks
	return h.Fake.StartDomain(name)
}

func (h *recorder) DestroyDomain(name string) error {
	h.w.add("DestroyDomain " + name)
	return h.Fake.DestroyDomain(name)
}

func (h *recorder) ShutdownDomain(name string) error {
	h.w.add("ShutdownDomain " + name)
	return h.Fake.ShutdownDomain(name)
}

func (h *recorder) RevertSnapshot(domain, name string) error {
	h.w.add("RevertSnapshot " + domain + " " + name)
	return h.Fake.RevertSnapshot(domain, name)
}

func (h *recorder) CreateSnapshot(domain, name, desc string) error {
	h.w.add("CreateSnapshot " + domain + " " + name)
	return h.Fake.CreateSnapshot(domain, name, desc)
}

func (h *recorder) DeleteSnapshot(domain, name string) error {
	h.w.add("DeleteSnapshot " + domain + " " + name)
	return h.Fake.DeleteSnapshot(domain, name)
}

func (h *recorder) DefineDomain(xml string) (virt.Domain, error) {
	d, err := h.Fake.DefineDomain(xml)
	h.w.add("DefineDomain " + d.Name)
	if h.w.screen != nil {
		h.SetScreenshot(d.Name, "image/png", h.w.screen)
	}
	return d, err
}

func (h *recorder) UndefineDomain(name string, flags virt.UndefineFlags) error {
	h.w.add("UndefineDomain " + name)
	return h.Fake.UndefineDomain(name, flags)
}

func (h *recorder) CreateVolumeFrom(pool, xml, fromPool, from string) (virt.Volume, error) {
	v, err := h.Fake.CreateVolumeFrom(pool, xml, fromPool, from)
	h.w.add("CreateVolumeFrom " + v.Name + " " + from)
	return v, err
}

func (h *recorder) DeleteVolume(pool, name string) error {
	h.w.add("DeleteVolume " + name)
	return h.Fake.DeleteVolume(pool, name)
}

func (h *recorder) SetDomainDescription(name, desc string) error {
	h.w.add("SetDomainDescription " + name)
	return h.Fake.SetDomainDescription(name, desc)
}

func (h *recorder) AttachDevice(domain, xml string) error {
	h.w.add("AttachDevice " + domain)
	return h.Fake.AttachDevice(domain, xml)
}

func (h *recorder) DetachDevice(domain, xml string) error {
	h.w.add("DetachDevice " + domain)
	return h.Fake.DetachDevice(domain, xml)
}

func (h *recorder) SendKey(domain string, keys []uint32) error {
	h.w.add(fmt.Sprintf("SendKey %s %v", domain, keys))
	if len(keys) == 1 && keys[0] == keyEnter {
		h.w.shell.locked = false
	}
	return h.Fake.SendKey(domain, keys)
}

func (h *recorder) DHCPLeases(network, mac string) ([]virt.DHCPLease, error) {
	if h.noLeases {
		return nil, nil
	}
	last, _ := strconv.ParseUint(mac[len(mac)-2:], 16, 8)
	if h.staleLeases {
		return []virt.DHCPLease{{MAC: mac, IP: fmt.Sprintf("192.168.122.%d", last), Expiry: 1}}, nil
	}
	return []virt.DHCPLease{{MAC: mac, IP: fmt.Sprintf("192.168.122.%d", last), Expiry: h.starts + 1}}, nil
}

// shell is the guest. It answers while a domain runs, logs "ssh <command>",
// and keeps the stdin of each call.
type shell struct {
	w       *world
	omarchy bool
	luks    bool
	// locks locks the disk at each start; locked holds the guest in its
	// initramfs, with no sshd, until Enter at the console opens the disk.
	locks, locked bool
	// systemd is the version systemctl --version prints; 0 prints nothing.
	systemd int
	// keyRejected makes sshd refuse every key until ssh-copy-id ran.
	keyRejected bool
	// userKeyOnly makes sshd accept the user's own key, and the werft key
	// only once it is in authorized_keys.
	userKeyOnly bool
	// fail makes a command that matches a pattern fail.
	fail map[string]bool
	// drop makes the connection of a command that matches a pattern drop
	// before the command ends.
	drop map[string]bool
	// onRun is called with each command before it runs.
	onRun func(cmd string)
	stdin []string
}

func (s *shell) running() []string {
	var out []string
	doms, _ := s.w.fake.Domains()
	for _, d := range doms {
		if st, _ := s.w.fake.DomainState(d.Name); st == virt.StateRunning {
			out = append(out, d.Name)
		}
	}
	return out
}

func (s *shell) Run(ctx context.Context, addr guest.Addr, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(s.running()) == 0 || addr.Host == "" || s.locked {
		return fmt.Errorf("ssh: connect to host %q: no route", addr.Host)
	}
	if s.keyRejected {
		return s.refuse(stderr)
	}
	s.w.add("ssh " + cmd)
	if stdin != nil {
		data, _ := io.ReadAll(stdin)
		s.stdin = append(s.stdin, cmd+"\x00"+string(data))
	}
	if s.onRun != nil {
		s.onRun(cmd)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for pattern := range s.drop {
		if regexp.MustCompile(pattern).MatchString(cmd) {
			return fmt.Errorf("ssh: connection to %s closed by remote host", addr.Host)
		}
	}
	for pattern := range s.fail {
		if regexp.MustCompile(pattern).MatchString(cmd) {
			return &guest.ExitError{Code: 1}
		}
	}
	switch {
	case strings.Contains(cmd, "/usr/share/omarchy ]"):
		return exit(s.omarchy)
	case strings.Contains(cmd, "grep -qx crypt"):
		return exit(s.luks)
	case strings.HasPrefix(cmd, "systemctl --version"):
		if s.systemd != 0 {
			_, _ = fmt.Fprintln(stdout, s.systemd)
		}
	case strings.Contains(cmd, "systemctl poweroff"):
		for _, d := range s.running() {
			if s.address(d) == addr.Host {
				s.w.fake.SetState(d, virt.StateShutOff)
			}
		}
		return fmt.Errorf("connection closed")
	case strings.Contains(cmd, "authorized_keys"):
		s.userKeyOnly = false
	case strings.Contains(cmd, "unlock-keyring.js"):
		_, _ = io.WriteString(stdout, "/org/freedesktop/secrets/collection/login\n")
	}
	return nil
}

// refuse is sshd's answer to a login with no key it knows; the log counts
// it as a failed login.
func (s *shell) refuse(stderr io.Writer) error {
	s.w.add("ssh refused")
	_, _ = io.WriteString(stderr, "tester@host: Permission denied (publickey,password).\n")
	return &guest.ExitError{Code: 255}
}

// address is the IP the recorder leases to the domain's MAC.
func (s *shell) address(domain string) string {
	if _, ips := virt.DomainAddresses(s.w.h, domain); len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// werftKeyShell is the guest as a login with the werft key alone sees it.
type werftKeyShell struct{ *shell }

func (s werftKeyShell) Run(ctx context.Context, addr guest.Addr, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	if s.userKeyOnly && len(s.running()) > 0 {
		return s.refuse(stderr)
	}
	return s.shell.Run(ctx, addr, cmd, stdin, stdout, stderr)
}

func exit(ok bool) error {
	if ok {
		return nil
	}
	return &guest.ExitError{Code: 1}
}

type host struct {
	w           *world
	display     bool
	stickErr    error
	holders     string
	viewers     int
	copyKeyErr  error
	settingsErr error

	// earlierViewer is a viewer an earlier run left open on the domain.
	earlierViewer bool
}

func (h *host) HasDisplay() bool { return h.display }

func (h *host) EarlierViewer(string) bool { return h.earlierViewer }

func (h *host) OpenViewer(domain string, reconnect bool) (func(), error) {
	h.viewers++
	h.w.add(fmt.Sprintf("viewer open %s reconnect=%v", domain, reconnect))
	return func() { h.w.add("viewer close " + domain) }, nil
}

func (h *host) CopyViewerSettings(from, to string) error {
	h.w.add("dconf copy")
	return h.settingsErr
}

func (h *host) StickMount(label string) string { return "/media/tester/" + label }

func (h *host) FindStick(mount string) (hostdev.Stick, error) {
	h.w.add("find stick " + mount)
	return hostdev.Stick{Device: "/dev/sdz1", UUID: "ABCD-1234", Vendor: "0781", Product: "5567"}, h.stickErr
}

func (h *host) StickHolders(hostdev.Stick) string { return h.holders }

func (h *host) UnmountStick(hostdev.Stick) error {
	h.w.add("host unmount stick")
	return nil
}

func (h *host) MountStick(hostdev.Stick) error {
	h.w.add("host mount stick")
	return nil
}

func (h *host) StickOnHost(hostdev.Stick) bool { return true }

func (h *host) CopySSHKey(addr guest.Addr, pubkey string) error {
	h.w.add("ssh-copy-id " + pubkey + " " + addr.String())
	if h.copyKeyErr != nil {
		return h.copyKeyErr
	}
	h.w.shell.keyRejected = false
	return nil
}

type terminal struct {
	w       *world
	tty     bool
	answer  bool
	secrets map[string]string
}

func (t *terminal) Confirm(q string) error {
	t.w.add("confirm " + q)
	if !t.tty {
		return failure.New(failure.NeedsConfirmation, "%s: no terminal; pass --yes", q)
	}
	if !t.answer {
		return failure.New(failure.NeedsConfirmation, "%s: not confirmed", q)
	}
	return nil
}

func (t *terminal) HasTerminal() bool { return t.tty }

func (t *terminal) ReadSecret(prompt string) (string, error) {
	t.w.add("secret " + prompt)
	if !t.tty {
		return "", failure.New(failure.SecretUnavailable, "%s: no terminal", prompt)
	}
	return t.secrets[prompt], nil
}

type passStore struct{ w *world }

func (p passStore) Lookup(attributes map[string]string) (string, error) {
	return "", fmt.Errorf("no keyring item %s", secret.Attributes(attributes))
}

func (p passStore) Show(entry string) (string, error) {
	p.w.add("pass show " + entry)
	if v, ok := p.w.pass[entry]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no entry %s", entry)
}

// assertSequence passes when the patterns match log entries in this order,
// each after the previous match, so a step that happens twice is checked at
// each place.
func (w *world) assertSequence(name string, patterns ...string) {
	w.t.Helper()
	log := w.entries()
	at := 0
	for _, p := range patterns {
		re := regexp.MustCompile("(?s)" + p)
		found := false
		for at < len(log) {
			at++
			if re.MatchString(log[at-1]) {
				found = true
				break
			}
		}
		if !found {
			w.t.Errorf("%s: no %q after the earlier patterns\nlog:\n  %s", name, p, strings.Join(log, "\n  "))
			return
		}
	}
}

// index is the place of the first log entry that matches pattern, -1 when
// none does.
func (w *world) index(pattern string) int {
	re := regexp.MustCompile("(?s)" + pattern)
	for i, e := range w.entries() {
		if re.MatchString(e) {
			return i
		}
	}
	return -1
}

func (w *world) count(pattern string) int {
	re := regexp.MustCompile("(?s)" + pattern)
	n := 0
	for _, e := range w.entries() {
		if re.MatchString(e) {
			n++
		}
	}
	return n
}

// setupRecipe loads testdata/setup/recipe.toml, a copy of the recipe of the
// setup repo with generic values.
func setupRecipe(t *testing.T) *recipe.Recipe {
	t.Helper()
	rec, err := recipe.Load(filepath.Join("testdata", "setup", "recipe.toml"))
	must(t, err)
	return rec
}
