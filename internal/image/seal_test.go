package image_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/image"
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
		return nil
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
		User: "hans", WerftKey: "ssh-ed25519 AAAAwerft",
		Global: func() (func(), error) { return func() {}, nil },
		Now:    func() time.Time { return time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC) },
		Sleep:  func(time.Duration) {},
		Out:    io.Discard,
	}
}

func TestPlanRefuses(t *testing.T) {
	secondDisk := strings.Replace(buildVM, "<disk type='file' device='cdrom'>",
		"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm.qcow2'/><target dev='vdb'/></disk><disk type='file' device='cdrom'>", 1)
	luks := strings.Replace(buildVM, "<name>vm</name>", "<name>vm</name><description>vm: the disk asks for the guest password at boot</description>", 1)
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
		{"full tier", "vm-full", "provision-base", buildVM, nil, failure.Usage},
		{"unknown VM", "none", "provision-base", buildVM, nil, failure.NotFound},
		{"running", "vm", "provision-base", buildVM, func(f *virttest.Fake) { f.SetState("vm", virt.StateRunning) }, failure.Conflict},
		{"no reset point", "vm", "provision-base", buildVM, func(f *virttest.Fake) { must(t, f.DeleteSnapshot("vm", "provision-base")) }, failure.NotFound},
		{"LUKS", "vm", "provision-base", luks, nil, failure.Unsupported},
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
		"LUKS": strings.Replace(buildVM, "<name>vm</name>", "<name>vm</name><description>vm: the disk asks for the guest password at boot</description>", 1),
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
		User: "hans", Family: "ubuntu", Systemd: 259,
	}
	if rec != want {
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
	if len(records) != 1 || records[0] != want {
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
