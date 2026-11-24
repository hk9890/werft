package image_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/guest/assets"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

const buildVM = `<domain type='kvm'><name>vm</name><memory unit='MiB'>1024</memory><os>
<type arch='x86_64' machine='q35'>hvm</type>
<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
<nvram format='qcow2'>/nvram/vm_VARS.qcow2</nvram></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm.qcow2'/><target dev='vda'/></disk>
<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><source file='/iso'/><target dev='sda'/><readonly/></disk>
<interface type='network'><mac address='52:54:00:00:00:01'/><source network='default'/></interface>
</devices></domain>`

// world is a shut-off build VM vm with the reset point provision-base, its
// disk in pool default and its NVRAM in pool nvram.
func world(t *testing.T) *virttest.Fake {
	return worldOf(t, buildVM)
}

// worldOf is world with the domain XML xml.
func worldOf(t *testing.T, xml string) *virttest.Fake {
	t.Helper()
	f := virttest.New()
	must(t, virt.EnsurePool(f, "default", "/img"))
	must(t, virt.EnsurePool(f, "nvram", "/nvram"))
	_, err := f.CreateVolume("default", "<volume><name>vm.qcow2</name><capacity unit='G'>20</capacity></volume>")
	must(t, err)
	_, err = f.CreateVolume("nvram", "<volume><name>vm_VARS.qcow2</name><capacity>0</capacity></volume>")
	must(t, err)
	_, err = f.DefineDomain(xml)
	must(t, err)
	must(t, f.CreateSnapshot("vm", "provision-base", ""))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// shell is the guest of the temporary domain: it answers the login probe,
// and for the seal script prints the guest facts and powers the domain off.
type shell struct {
	f        *virttest.Fake
	sealErr  error
	loginErr error
	commands []string
	// xml is the inactive XML of the temporary domain at the seal script.
	xml string
}

func (s *shell) Run(_ context.Context, addr guest.Addr, cmd string, _ io.Reader, stdout, _ io.Writer) error {
	s.commands = append(s.commands, addr.String()+" "+strings.Fields(cmd)[0])
	var name string
	for _, c := range s.f.Calls() {
		if after, ok := strings.CutPrefix(c, "StartDomain "); ok {
			name = after
		}
	}
	if !strings.HasPrefix(cmd, "sh -c") {
		return s.loginErr
	}
	s.xml, _ = s.f.DomainXML(name, true)
	if s.sealErr != nil {
		return s.sealErr
	}
	_, _ = io.WriteString(stdout, "ubuntu\n259\n")
	return s.f.DestroyDomain(name)
}

func env(t *testing.T, f *virttest.Fake, sh guest.Shell) image.Env {
	return image.Env{
		Ctx: context.Background(), H: f, Shell: sh, Key: "ssh-ed25519 AAAAseal seal",
		Pool: "werft", PoolPath: "/werft", Catalog: filepath.Join(t.TempDir(), "images.toml"),
		User: "tester", WerftKey: "ssh-ed25519 AAAAwerft",
		Global: func() (func(), error) { return func() {}, nil },
		Now:    func() time.Time { return time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC) },
		Sleep:  func(time.Duration) {},
		Out:    io.Discard,
	}
}

func TestPlanRefuses(t *testing.T) {
	secondDisk := strings.Replace(buildVM, "<disk type='file' device='cdrom'>",
		"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm.qcow2'/><target dev='vdb'/></disk><disk type='file' device='cdrom'>", 1)
	bios := strings.Replace(strings.Replace(buildVM, "<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>", "", 1),
		"<nvram format='qcow2'>/nvram/vm_VARS.qcow2</nvram>", "", 1)
	tests := []struct {
		name   string
		vm     string
		label  string
		xml    string
		before func(f *virttest.Fake)
		want   failure.Kind
	}{
		{"other label", "vm", "clean-install", buildVM, nil, failure.Usage},
		{"provision-base of a -full build VM", "vm-full", "provision-base", buildVM, nil, failure.Usage},
		{"clean of a build VM that is not -full", "vm", "clean", buildVM, nil, failure.Usage},
		{"clean of a build VM named only -full", "-full", "clean", buildVM, nil, failure.Usage},
		{"unknown VM", "none", "provision-base", buildVM, nil, failure.NotFound},
		{"running", "vm", "provision-base", buildVM, func(f *virttest.Fake) { f.SetState("vm", virt.StateRunning) }, failure.Conflict},
		{"no reset point", "vm", "provision-base", buildVM, func(f *virttest.Fake) { must(t, f.DeleteSnapshot("vm", "provision-base")) }, failure.NotFound},
		{"two writable disks", "vm", "provision-base", secondDisk, nil, failure.Unsupported},
		{"no UEFI NVRAM", "vm", "provision-base", bios, nil, failure.Unsupported},
		{"disk in no pool", "vm", "provision-base", strings.Replace(buildVM, "/img/vm.qcow2", "/elsewhere/vm.qcow2", 1), nil, failure.NotFound},
		{"NVRAM in no pool", "vm", "provision-base", strings.Replace(buildVM, "/nvram/vm_VARS.qcow2", "/elsewhere/vm_VARS.qcow2", 1), nil, failure.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := worldOf(t, tt.xml)
			if tt.before != nil {
				tt.before(f)
			}
			before := len(f.Calls())
			if _, err := image.Plan(f, tt.vm, tt.label); failure.KindOf(err) != tt.want {
				t.Errorf("Plan = %v, want kind %q", err, tt.want)
			}
			for _, c := range f.Calls()[before:] {
				if !readOnly(c) {
					t.Errorf("Plan changed something: %s", c)
				}
			}
		})
	}
}

// TestPlanChecksTheResetPoint gives the reset point a definition that seal
// refuses, and the build VM a current one that passes. The revert brings
// back the refused one, so Plan must refuse before any change.
func TestPlanChecksTheResetPoint(t *testing.T) {
	tests := map[string]string{
		"two writable disks": strings.Replace(buildVM, "<disk type='file' device='cdrom'>",
			"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm.qcow2'/><target dev='vdb'/></disk><disk type='file' device='cdrom'>", 1),
	}
	for name, atResetPoint := range tests {
		t.Run(name, func(t *testing.T) {
			f := worldOf(t, atResetPoint)
			d, err := f.Domain("vm")
			must(t, err)
			_, err = f.DefineDomain(strings.Replace(buildVM, "<name>vm</name>", "<name>vm</name><uuid>"+d.UUID+"</uuid>", 1))
			must(t, err)
			before := len(f.Calls())
			if _, err := image.Plan(f, "vm", "provision-base"); !failure.Is(err, failure.Unsupported) {
				t.Errorf("Plan = %v, want unsupported", err)
			}
			for _, c := range f.Calls()[before:] {
				if !readOnly(c) {
					t.Errorf("Plan changed something: %s", c)
				}
			}
		})
	}
}

func withSystemd(v int) string {
	return strings.Replace(buildVM, "<name>vm</name>", "<name>vm</name><description>my notes\n"+domxml.SystemdLine(v)+"</description>", 1)
}

// TestSealRefusesSystemdBelow255: a reset point that records systemd 254 is
// refused at once, before the revert and any volume copy.
func TestSealRefusesSystemdBelow255(t *testing.T) {
	f := worldOf(t, withSystemd(254))
	before := len(f.Calls())
	_, err := image.Plan(f, "vm", "provision-base")
	if !failure.Is(err, failure.Unsupported) || !strings.Contains(err.Error(), "has systemd 254") {
		t.Fatalf("Plan = %v, want unsupported for systemd 254", err)
	}
	for _, c := range f.Calls()[before:] {
		if !readOnly(c) {
			t.Errorf("seal of a systemd 254 guest changed something: %s", c)
		}
	}
}

// tcpShell is the guest of a systemd 255 seal. It keeps the seal command,
// and with noPoweroff the guest stays up after the script.
type tcpShell struct {
	shell
	script     string
	noPoweroff bool
}

func (s *tcpShell) Run(ctx context.Context, addr guest.Addr, cmd string, in io.Reader, stdout, stderr io.Writer) error {
	if !strings.HasPrefix(cmd, "sh -c") {
		return s.shell.Run(ctx, addr, cmd, in, stdout, stderr)
	}
	s.script = cmd
	s.commands = append(s.commands, addr.String()+" sh")
	name := "vm-plain-20261102"
	s.xml, _ = s.f.DomainXML(name, true)
	if s.sealErr != nil {
		return s.sealErr
	}
	_, _ = io.WriteString(stdout, "ubuntu\n255\n")
	if s.noPoweroff {
		return nil
	}
	return s.f.DestroyDomain(name)
}

// dhcp is the fake that gives the running temporary domain a DHCP lease.
type dhcp struct{ *virttest.Fake }

func (d dhcp) StartDomain(name string) error {
	if err := d.Fake.StartDomain(name); err != nil {
		return err
	}
	text, err := d.DomainXML(name, false)
	if err != nil {
		return err
	}
	parsed, err := domxml.Parse(text)
	if err != nil {
		return err
	}
	d.AddDHCPLease("default", virt.DHCPLease{MAC: domxml.Interfaces(parsed)[0].MAC, IP: "192.168.122.50"})
	return nil
}

// credentialsOf decodes the SMBIOS credentials of a domain definition.
func credentialsOf(t *testing.T, text string) map[string]string {
	t.Helper()
	d, err := domxml.Parse(text)
	must(t, err)
	creds := map[string]string{}
	if len(d.SysInfo) == 0 {
		return creds
	}
	for _, entry := range d.SysInfo[0].SMBIOS.OEMStrings.Entry {
		name, value, _ := strings.Cut(strings.TrimPrefix(entry, "io.systemd.credential.binary:"), "=")
		data, err := base64.StdEncoding.DecodeString(value)
		must(t, err)
		creds[name] = string(data)
	}
	return creds
}

func TestSeal255LogsInOverDHCP(t *testing.T) {
	f := worldOf(t, withSystemd(255))
	sh := &tcpShell{shell: shell{f: f}}
	e := env(t, f, sh)
	e.H = dhcp{f}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	rec, err := s.Run(e)
	must(t, err)

	if rec.Systemd != 255 {
		t.Errorf("record systemd = %d", rec.Systemd)
	}
	if strings.Contains(rec.Domain, "<vsock") || strings.Contains(rec.Domain, "io.systemd.credential") {
		t.Errorf("the stock definition has a vsock device or credentials:\n%s", rec.Domain)
	}
	if strings.Contains(sh.xml, "<vsock") {
		t.Errorf("the temporary domain has a vsock device:\n%s", sh.xml)
	}
	line := base64.StdEncoding.EncodeToString([]byte("\nssh-ed25519 AAAAseal seal\n"))
	want := map[string]string{"tmpfiles.extra": "d /root/.ssh 0700 root root -\n" +
		"f /root/.ssh/authorized_keys 0600 root root -\n" +
		"w+~ /root/.ssh/authorized_keys - - - - " + line + "\n"}
	if got := credentialsOf(t, sh.xml); !reflect.DeepEqual(got, want) {
		t.Errorf("credentials = %q, want %q", got, want)
	}
	if got := strings.Join(sh.commands, ", "); got != "root@192.168.122.50 true, root@192.168.122.50 sh" {
		t.Errorf("guest commands = %s", got)
	}
	if want := guest.ScriptCommand(assets.SealTCP+assets.Seal, "tester", "ssh-ed25519 AAAAwerft", "ssh-ed25519 AAAAseal seal"); sh.script != want {
		t.Errorf("seal command = %s", sh.script)
	}
}

// TestSeal255FailureRemovesTheCopy: each failure keeps its kind and removes
// the temporary domain and both volumes.
func TestSeal255FailureRemovesTheCopy(t *testing.T) {
	tests := []struct {
		name  string
		lease bool
		sh    tcpShell
		want  failure.Kind
		hint  string
	}{
		{name: "no DHCP lease", want: failure.Timeout, hint: "DHCP configuration"},
		{name: "login timeout", lease: true, sh: tcpShell{shell: shell{loginErr: errors.New("connection refused")}}, want: failure.Timeout, hint: "PermitRootLogin"},
		{name: "script failure", lease: true, sh: tcpShell{shell: shell{sealErr: &guest.ExitError{Code: 1}}}, want: failure.Failed, hint: "seal script failed"},
		{name: "no poweroff", lease: true, sh: tcpShell{noPoweroff: true}, want: failure.Timeout, hint: "did not power off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := worldOf(t, withSystemd(255))
			sh := tt.sh
			sh.f = f
			e := env(t, f, &sh)
			if tt.lease {
				e.H = dhcp{f}
			}
			s, err := image.Plan(f, "vm", "provision-base")
			must(t, err)
			_, err = s.Run(e)
			if !failure.Is(err, tt.want) || !strings.Contains(err.Error(), tt.hint) || !strings.Contains(err.Error(), "domain and volumes are removed") {
				t.Fatalf("Run = %v, want kind %s with %q", err, tt.want, tt.hint)
			}
			if _, err := f.Domain("vm-plain-20261102"); !failure.Is(err, failure.NotFound) {
				t.Errorf("the temporary domain is left: %v", err)
			}
			if names, _ := f.Volumes("werft"); len(names) != 0 {
				t.Errorf("volumes left: %q", names)
			}
		})
	}
}

// TestPlan255NeedsALibvirtNetwork: the TCP login finds its address in a
// libvirt DHCP lease, so a guest without such an interface is refused before
// the revert. A vsock guest has no such need.
func TestPlan255NeedsALibvirtNetwork(t *testing.T) {
	const network = "<interface type='network'><mac address='52:54:00:00:00:01'/><source network='default'/></interface>"
	for name, iface := range map[string]string{
		"bridge":       "<interface type='bridge'><mac address='52:54:00:00:00:01'/><source bridge='br0'/></interface>",
		"direct":       "<interface type='direct'><mac address='52:54:00:00:00:01'/><source dev='eth0' mode='bridge'/></interface>",
		"no interface": "",
	} {
		t.Run(name, func(t *testing.T) {
			f := worldOf(t, strings.Replace(withSystemd(255), network, iface, 1))
			before := len(f.Calls())
			if _, err := image.Plan(f, "vm", "provision-base"); !failure.Is(err, failure.Unsupported) || !strings.Contains(err.Error(), "libvirt network") {
				t.Fatalf("Plan = %v, want unsupported for the network", err)
			}
			for _, c := range f.Calls()[before:] {
				if !readOnly(c) {
					t.Errorf("Plan changed something: %s", c)
				}
			}
			f = worldOf(t, strings.Replace(withSystemd(256), network, iface, 1))
			if _, err := image.Plan(f, "vm", "provision-base"); err != nil {
				t.Errorf("Plan at systemd 256 = %v", err)
			}
		})
	}
}

// liveXMLTimeout fails the read of the running definition with a timeout.
type liveXMLTimeout struct{ *virttest.Fake }

func (l liveXMLTimeout) DomainXML(name string, inactive bool) (string, error) {
	if !inactive {
		return "", failure.New(failure.Timeout, "XML of %s: libvirt operation timeout", name)
	}
	return l.Fake.DomainXML(name, inactive)
}

// TestSealVsockAddressErrorHasNoLoginHint: a libvirt failure to read the CID
// is no failed login, so it gets none of the login hints.
func TestSealVsockAddressErrorHasNoLoginHint(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{f: f})
	e.H = liveXMLTimeout{f}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	_, err = s.Run(e)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "libvirt operation timeout") {
		t.Fatalf("Run = %v, want the libvirt timeout", err)
	}
	if strings.Contains(err.Error(), "records no systemd version") {
		t.Errorf("Run = %v, has a login hint", err)
	}
}

// TestSeal256KeepsTheVsockPath pins the credentials, the login and the
// command of a guest with SSH over vsock.
func TestSeal256KeepsTheVsockPath(t *testing.T) {
	for _, xml := range []string{withSystemd(256), withSystemd(259), buildVM} {
		f := worldOf(t, xml)
		sh := &scriptShell{shell: shell{f: f}}
		s, err := image.Plan(f, "vm", "provision-base")
		must(t, err)
		_, err = s.Run(env(t, f, sh))
		must(t, err)
		want := map[string]string{
			"ssh.ephemeral-authorized_keys-all":       "ssh-ed25519 AAAAseal seal\n",
			"systemd.unit-dropin.sshd-vsock@.service": "[Service]\nExecStartPre=/usr/bin/ssh-keygen -A\n",
		}
		if got := credentialsOf(t, sh.xml); !reflect.DeepEqual(got, want) {
			t.Errorf("credentials = %q, want %q", got, want)
		}
		if got := strings.Join(sh.commands, ", "); got != "root@vsock/3 true, root@vsock/3 sh" {
			t.Errorf("guest commands = %s", got)
		}
		if want := guest.ScriptCommand(assets.Seal, "tester", "ssh-ed25519 AAAAwerft"); sh.script != want {
			t.Errorf("seal command = %s", sh.script)
		}
	}
}

// scriptShell is shell that keeps the seal command.
type scriptShell struct {
	shell
	script string
}

func (s *scriptShell) Run(ctx context.Context, addr guest.Addr, cmd string, in io.Reader, stdout, stderr io.Writer) error {
	if strings.HasPrefix(cmd, "sh -c") {
		s.script = cmd
	}
	return s.shell.Run(ctx, addr, cmd, in, stdout, stderr)
}

// TestSealRefusesASystemdLineTheRevertChanged: the login path is planned from
// the line of the reset point, so another line after the revert stops the
// seal before any copy.
func TestSealRefusesASystemdLineTheRevertChanged(t *testing.T) {
	f := worldOf(t, withSystemd(255))
	e := env(t, f, &shell{f: f})
	e.H = redefine{f, withSystemd(259)}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	if _, err := s.Run(e); !failure.Is(err, failure.Failed) || !strings.Contains(err.Error(), "systemd line") {
		t.Fatalf("Run = %v, want failed for the systemd line", err)
	}
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "CreateVolumeFrom ") || strings.HasPrefix(c, "StartDomain ") {
			t.Errorf("Run went on after the changed line: %s", c)
		}
	}
}

// TestSealTimeoutHintsAtMissingSystemdLine: a reset point without the
// systemd line is sealed as before. When the vsock login then times out, the
// error tells how to let seal check the version before the copy. With the
// line recorded, the hint stays away.
func TestSealTimeoutHintsAtMissingSystemdLine(t *testing.T) {
	const hint = "records no systemd version; run werft prepare vm and take the reset point again"
	tests := map[string]struct {
		xml      string
		wantHint bool
	}{
		"no systemd line": {buildVM, true},
		"systemd 256":     {strings.Replace(buildVM, "<name>vm</name>", "<name>vm</name><description>"+domxml.SystemdLine(256)+"</description>", 1), false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := worldOf(t, tt.xml)
			s, err := image.Plan(f, "vm", "provision-base")
			must(t, err)
			_, err = s.Run(env(t, f, &shell{f: f, loginErr: errors.New("connection refused")}))
			if !failure.Is(err, failure.Timeout) {
				t.Fatalf("Run = %v, want timeout", err)
			}
			if got := strings.Contains(err.Error(), hint); got != tt.wantHint {
				t.Errorf("Run = %v, hint shown %v, want %v", err, got, tt.wantHint)
			}
		})
	}
}

func readOnly(call string) bool {
	for _, prefix := range []string{"LibVersion", "DomainState", "DomainXML", "Snapshots", "SnapshotXML", "DomainDescription", "VolumeByPath"} {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func TestSealWritesVolumesAndRecord(t *testing.T) {
	f := world(t)
	sh := &shell{f: f}
	e := env(t, f, sh)
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	rec, err := s.Run(e)
	must(t, err)

	want := image.Record{
		Name: "vm-plain-20261102", Image: "vm", Tier: "plain", Version: "20261102", Date: e.Now(),
		Source: "vm", Label: "provision-base", Pool: "werft", Disk: "vm-plain-20261102.qcow2", NVRAM: "vm-plain-20261102_VARS.qcow2",
		User: "tester", Family: "ubuntu", Systemd: 259,
	}
	// The stock definition is the temporary domain without its credentials.
	if !strings.Contains(rec.Domain, "/werft/vm-plain-20261102.qcow2") || !strings.Contains(rec.Domain, "<vsock") ||
		strings.Contains(rec.Domain, "io.systemd.credential") {
		t.Errorf("stock definition = %s", rec.Domain)
	}
	want.Domain = rec.Domain
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("record = %+v, want %+v", rec, want)
	}
	if _, err := f.Domain(rec.Name); !failure.Is(err, failure.NotFound) {
		t.Errorf("the temporary domain is left: %v", err)
	}
	for _, v := range []string{rec.Disk, rec.NVRAM} {
		if _, err := f.VolumePath("werft", v); err != nil {
			t.Errorf("stock volume %s: %v", v, err)
		}
	}
	for _, part := range []string{"/werft/vm-plain-20261102.qcow2", "/werft/vm-plain-20261102_VARS.qcow2", "<vsock model=\"virtio\">",
		"io.systemd.credential.binary:ssh.ephemeral-authorized_keys-all=", "io.systemd.credential.binary:systemd.unit-dropin.sshd-vsock@.service="} {
		if !strings.Contains(sh.xml, part) {
			t.Errorf("the temporary domain lacks %s:\n%s", part, sh.xml)
		}
	}
	if strings.Contains(sh.xml, "52:54:00:00:00:01") {
		t.Errorf("the temporary domain has the MAC of the build VM")
	}
	// No login after the seal script: it would start an sshd that makes new
	// host keys.
	if got := strings.Join(sh.commands, ", "); got != "root@vsock/3 true, root@vsock/3 sh" {
		t.Errorf("guest commands = %s", got)
	}
	records, err := image.Load(e.Catalog)
	must(t, err)
	if len(records) != 1 || !reflect.DeepEqual(records[0], want) {
		t.Errorf("catalog = %+v", records)
	}

	again, err := s.Run(e)
	must(t, err)
	if again.Name != "vm-plain-20261102-2" || again.Version != "20261102-2" {
		t.Errorf("second seal of the day = %s, version %s", again.Name, again.Version)
	}
	if records, _ := image.Load(e.Catalog); len(records) != 2 {
		t.Errorf("catalog after two seals = %+v", records)
	}
}

// TestSealFullTier seals the clean reset point of a -full build VM: the
// record has the image without -full and the tier full.
func TestSealFullTier(t *testing.T) {
	f := worldOf(t, buildVM)
	_, err := f.DefineDomain(strings.Replace(buildVM, "<name>vm</name>", "<name>vm-full</name>", 1))
	must(t, err)
	must(t, f.CreateSnapshot("vm-full", "clean", ""))
	e := env(t, f, &shell{f: f})
	s, err := image.Plan(f, "vm-full", "clean")
	must(t, err)
	if s.Image != "vm" || s.Tier != image.Full {
		t.Errorf("plan = image %s, tier %s; want image vm, tier full", s.Image, s.Tier)
	}
	rec, err := s.Run(e)
	must(t, err)
	if rec.Name != "vm-full-20261102" || rec.Image != "vm" || rec.Tier != "full" || rec.Source != "vm-full" || rec.Label != "clean" ||
		rec.Disk != "vm-full-20261102.qcow2" || rec.NVRAM != "vm-full-20261102_VARS.qcow2" {
		t.Errorf("record = %+v", rec)
	}
	if _, err := image.Plan(f, "vm-full", "provision-base"); !failure.Is(err, failure.Usage) {
		t.Errorf("Plan of provision-base on vm-full = %v, want usage", err)
	}
}

// TestSealUsesTheDomainOfTheResetPoint changes the definition after the
// reset point: the revert brings back the one of the reset point, and the
// temporary domain must have that.
func TestSealUsesTheDomainOfTheResetPoint(t *testing.T) {
	f := world(t)
	d, err := f.Domain("vm")
	must(t, err)
	changed := strings.Replace(buildVM, "<name>vm</name><memory unit='MiB'>1024</memory>", "<name>vm</name><uuid>"+d.UUID+"</uuid><memory unit='MiB'>4096</memory>", 1)
	_, err = f.DefineDomain(changed)
	must(t, err)
	sh := &shell{f: f}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	_, err = s.Run(env(t, f, sh))
	must(t, err)
	if !strings.Contains(sh.xml, `<memory unit="MiB">1024</memory>`) {
		t.Errorf("the temporary domain does not have the definition of the reset point:\n%s", sh.xml)
	}
}

func TestSealFailureRemovesTheCopy(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{f: f, sealErr: &guest.ExitError{Code: 1}})
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	_, err = s.Run(e)
	var exit *guest.ExitError
	if !errors.As(err, &exit) || !strings.Contains(err.Error(), "domain and volumes are removed") {
		t.Fatalf("Run = %v, want the failed seal script", err)
	}
	if _, err := f.Domain("vm-plain-20261102"); !failure.Is(err, failure.NotFound) {
		t.Errorf("the temporary domain is left: %v", err)
	}
	if names, _ := f.Volumes("werft"); len(names) != 0 {
		t.Errorf("volumes left after a failed seal: %q", names)
	}
	if records, _ := image.Load(e.Catalog); len(records) != 0 {
		t.Errorf("a failed seal wrote %+v", records)
	}
}

func TestFailedNVRAMCopyRemovesTheDiskCopy(t *testing.T) {
	f := world(t)
	f.FailOn("CreateVolumeFrom werft vm-plain-20261102_VARS.qcow2", failure.New(failure.Failed, "copy failed"))
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	if _, err := s.Run(env(t, f, &shell{f: f})); !strings.Contains(fmtRun(err), "copy failed") {
		t.Fatalf("Run = %v, want the failed NVRAM copy", err)
	}
	if names, _ := f.Volumes("werft"); len(names) != 0 {
		t.Errorf("volumes left after a failed NVRAM copy: %q", names)
	}
	if _, err := f.Domain("vm-plain-20261102"); !failure.Is(err, failure.NotFound) {
		t.Errorf("the temporary domain is left: %v", err)
	}
}

func TestFailedCleanupNamesWhatIsLeft(t *testing.T) {
	f := world(t)
	f.FailOn("CreateVolumeFrom werft vm-plain-20261102_VARS.qcow2", failure.New(failure.Failed, "copy failed"))
	f.FailOn("DeleteVolume werft vm-plain-20261102.qcow2", failure.New(failure.Failed, "pool busy"))
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	_, err = s.Run(env(t, f, &shell{f: f}))
	if msg := fmtRun(err); !strings.Contains(msg, "remove what is left by hand: volume vm-plain-20261102.qcow2 in pool werft (delete: pool busy)") {
		t.Errorf("Run = %s", msg)
	}
}

func fmtRun(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// luksVM is buildVM with the LUKS marker vm and prepare set.
var luksVM = strings.Replace(buildVM, "<name>vm</name>", "<name>vm</name><description>my notes\n"+domxml.LUKSMarker+"</description>", 1)

// screen is the fake whose every console shows a still prompt.
type screen struct{ *virttest.Fake }

func (s screen) Screenshot(domain string, w io.Writer) (string, error) {
	if _, err := s.Fake.Screenshot(domain, io.Discard); err != nil {
		return "", err
	}
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.White)
	return "image/png", png.Encode(w, img)
}

// store is pass with one entry.
type store map[string]string

// Lookup finds a keyring item under its attributes as one key.
func (s store) Lookup(attributes map[string]string) (string, error) {
	return s.Show(secret.Attributes(attributes))
}

func (s store) Show(entry string) (string, error) {
	if v, ok := s[entry]; ok {
		return v, nil
	}
	return "", failure.New(failure.SecretUnavailable, "pass show %s failed", entry)
}

// noTerminal is stdin that is no terminal.
type noTerminal struct{}

func (noTerminal) ReadSecret(prompt string) (string, error) {
	return "", failure.New(failure.SecretUnavailable, "%s: stdin is no terminal to ask on", prompt)
}

// TestPlanReadsTheLUKSMarkerOfTheResetPoint: the revert brings back the
// definition of the reset point, so its marker counts.
func TestPlanReadsTheLUKSMarkerOfTheResetPoint(t *testing.T) {
	f := worldOf(t, luksVM)
	d, err := f.Domain("vm")
	must(t, err)
	_, err = f.DefineDomain(strings.Replace(buildVM, "<name>vm</name>", "<name>vm</name><uuid>"+d.UUID+"</uuid>", 1))
	must(t, err)
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	if !s.LUKS {
		t.Errorf("Plan of a reset point with the LUKS marker: LUKS = false")
	}
}

func TestSealTypesTheDiskPassword(t *testing.T) {
	f := worldOf(t, luksVM)
	sh := &shell{f: f}
	e := env(t, f, sh)
	e.H = screen{f}
	var out bytes.Buffer
	e.Out = &out
	keyring := map[string]string{"service": "werft", "username": "tester"}
	e.DiskPassword = secret.Spec{Keyring: keyring, Pass: "werft/guest-password", Prompt: "Disk password of vm"}
	e.Secrets, e.Terminal = store{"service=werft username=tester": "pw"}, noTerminal{}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	rec, err := s.Run(e)
	must(t, err)

	if !rec.LUKS || rec.DiskPasswordPass != "werft/guest-password" || !reflect.DeepEqual(rec.DiskPasswordKeyring, keyring) {
		t.Errorf("record = %+v, want the LUKS marker, the keyring item and the pass entry", rec)
	}
	records, err := image.Load(e.Catalog)
	must(t, err)
	if len(records) != 1 || !records[0].LUKS || records[0].DiskPasswordPass != "werft/guest-password" || !reflect.DeepEqual(records[0].DiskPasswordKeyring, keyring) {
		t.Errorf("catalog = %+v", records)
	}
	// p, w, Enter: once, before the first login.
	var keys []string
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "SendKey ") || strings.HasPrefix(c, "StartDomain ") {
			keys = append(keys, c)
		}
	}
	want := []string{"StartDomain vm-plain-20261102", "SendKey vm-plain-20261102", "SendKey vm-plain-20261102", "SendKey vm-plain-20261102"}
	if !slices.Equal(keys, want) {
		t.Errorf("start and keys = %q, want %q", keys, want)
	}
	if strings.Contains(out.String(), "pw\n") || strings.Contains(rec.Domain, "pw") {
		t.Errorf("the password shows in the output or the record:\n%s", out.String())
	}
}

func TestSealRefusesWithoutAPasswordSource(t *testing.T) {
	tests := map[string]secret.Spec{
		"no pass entry":       {Prompt: "Disk password of vm"},
		"pass cannot give it": {Pass: "vm/missing", Prompt: "Disk password of vm"},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			f := worldOf(t, luksVM)
			e := env(t, f, &shell{f: f})
			e.DiskPassword, e.Secrets, e.Terminal = spec, store{}, noTerminal{}
			s, err := image.Plan(f, "vm", "provision-base")
			must(t, err)
			before := len(f.Calls())
			if _, err := s.Run(e); !failure.Is(err, failure.SecretUnavailable) {
				t.Fatalf("Run = %v, want secret_unavailable", err)
			}
			for _, c := range f.Calls()[before:] {
				t.Errorf("Run without a password changed or read libvirt: %s", c)
			}
		})
	}
}

func TestSealRefusesAnUntypeablePassword(t *testing.T) {
	f := worldOf(t, luksVM)
	e := env(t, f, &shell{f: f})
	e.DiskPassword, e.Secrets, e.Terminal = secret.Spec{Pass: "werft/guest-password"}, store{"werft/guest-password": "pässword"}, noTerminal{}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	before := len(f.Calls())
	if _, err := s.Run(e); !failure.Is(err, failure.Usage) || strings.Contains(err.Error(), "pässword") {
		t.Fatalf("Run = %v, want usage without the password", err)
	}
	if calls := f.Calls()[before:]; len(calls) != 0 {
		t.Errorf("Run changed something: %q", calls)
	}
}

// redefine is the fake whose revert leaves the build VM with the
// definition xml, not the one of the reset point.
type redefine struct {
	*virttest.Fake
	xml string
}

func (r redefine) RevertSnapshot(domain, name string) error {
	if err := r.Fake.RevertSnapshot(domain, name); err != nil {
		return err
	}
	d, err := r.Domain(domain)
	if err != nil {
		return err
	}
	_, err = r.DefineDomain(strings.Replace(r.xml, "<name>vm</name>", "<name>vm</name><uuid>"+d.UUID+"</uuid>", 1))
	return err
}

// TestSealRefusesALUKSMarkerTheRevertChanged: the password is read for the
// marker of the reset point; a definition after the revert with another
// marker would boot without the password or type it at no prompt.
func TestSealRefusesALUKSMarkerTheRevertChanged(t *testing.T) {
	tests := map[string]struct{ atResetPoint, afterRevert string }{
		"marker lost":   {luksVM, buildVM},
		"marker gained": {buildVM, luksVM},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := worldOf(t, tt.atResetPoint)
			e := env(t, f, &shell{f: f})
			e.H = redefine{f, tt.afterRevert}
			e.DiskPassword, e.Secrets, e.Terminal = secret.Spec{Pass: "werft/guest-password"}, store{"werft/guest-password": "pw"}, noTerminal{}
			s, err := image.Plan(f, "vm", "provision-base")
			must(t, err)
			if _, err := s.Run(e); !failure.Is(err, failure.Failed) || !strings.Contains(err.Error(), "LUKS marker") {
				t.Fatalf("Run = %v, want failed for the LUKS marker", err)
			}
			for _, c := range f.Calls() {
				if strings.HasPrefix(c, "CreateVolumeFrom ") || strings.HasPrefix(c, "DefineDomain vm-") || strings.HasPrefix(c, "StartDomain ") {
					t.Errorf("Run went on after the changed marker: %s", c)
				}
			}
		})
	}
}

// silent is a guest that never answers SSH, as after a refused password.
type silent struct{}

func (silent) Run(context.Context, guest.Addr, string, io.Reader, io.Writer, io.Writer) error {
	return errors.New("connection refused")
}

// TestSealOfARefusedPasswordNamesNoRemovedDomain: the failed seal removes
// the temporary domain, so the error must not send the user to its console.
func TestSealOfARefusedPasswordNamesNoRemovedDomain(t *testing.T) {
	f := worldOf(t, luksVM)
	e := env(t, f, silent{})
	e.H = screen{f}
	e.DiskPassword, e.Secrets, e.Terminal = secret.Spec{Pass: "werft/guest-password"}, store{"werft/guest-password": "pw"}, noTerminal{}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	_, err = s.Run(e)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "refused the typed password") {
		t.Fatalf("Run = %v, want a timeout that names the password", err)
	}
	if strings.Contains(err.Error(), "virt-viewer") {
		t.Errorf("Run names the console of the removed domain: %v", err)
	}
}

// TestSealNamesTheVersionByTheUTCDate: a seal just after local midnight
// east of UTC takes the UTC date, the date of its record.
func TestSealNamesTheVersionByTheUTCDate(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{f: f})
	e.Now = func() time.Time { return time.Date(2026, 11, 19, 0, 36, 0, 0, time.FixedZone("CET", 3600)) }
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	rec, err := s.Run(e)
	must(t, err)
	if rec.Name != "vm-plain-20261118" || rec.Version != "20261118" {
		t.Errorf("seal at 00:36 CET = %s, version %s, want the UTC date 20261118", rec.Name, rec.Version)
	}
	if want := time.Date(2026, 11, 18, 23, 36, 0, 0, time.UTC); !rec.Date.Equal(want) || rec.Date.Location() != time.UTC {
		t.Errorf("date = %v, want %v", rec.Date, want)
	}
}

// TestSealRefusesACatalogWithUnknownKeys: a newer werft wrote images.toml,
// and a write by this werft would delete the keys it does not know.
func TestSealRefusesACatalogWithUnknownKeys(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{f: f})
	content := []byte("[[image]]\nname = \"vm-plain-20261101\"\nimage = \"vm\"\ntier = \"plain\"\nnewer_field = \"x\"\n")
	must(t, os.WriteFile(e.Catalog, content, 0o600))
	records, err := image.Load(e.Catalog)
	if err != nil || len(records) != 1 {
		t.Fatalf("Load = %+v, %v", records, err)
	}
	s, err := image.Plan(f, "vm", "provision-base")
	must(t, err)
	_, err = s.Run(e)
	if !failure.Is(err, failure.Unsupported) || !strings.Contains(err.Error(), "image.newer_field") || !strings.Contains(err.Error(), "update werft") {
		t.Errorf("seal = %v, want unsupported naming image.newer_field", err)
	}
	if got, _ := os.ReadFile(e.Catalog); !bytes.Equal(got, content) {
		t.Errorf("images.toml changed:\n%s", got)
	}
	if _, err := f.Domain("vm-plain-20261102"); !failure.Is(err, failure.NotFound) {
		t.Errorf("seal defined a temporary domain: %v", err)
	}
	if calls := strings.Join(f.Calls(), "\n"); strings.Contains(calls, "RevertSnapshot") {
		t.Errorf("seal reset the build VM before the refusal:\n%s", calls)
	}
}
