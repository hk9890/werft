package lifecycle_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/lifecycle"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// buildVM is a build VM as vm left it: raw NVRAM in nvramDir, a qcow2 disk
// in the default pool, an installer ISO, one NIC.
func buildVM(name, nvramDir string) string {
	return `<domain type='kvm'><name>` + name + `</name><os>
<type arch='x86_64' machine='q35'>hvm</type>
<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
<nvram format='raw'>` + nvramDir + `/` + name + `_VARS.fd</nvram></os><devices>
<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><source file='/iso/install.iso'/><target dev='sda'/><readonly/></disk>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/` + name + `.qcow2'/><target dev='vda'/></disk>
<interface type='network'><mac address='52:54:00:00:00:01'/><source network='default'/></interface>
</devices></domain>`
}

const vsockVM = `<domain type='kvm'><name>werft-a</name><os><type>hvm</type></os><devices>
<interface type='network'><mac address='52:54:00:00:00:02'/><source network='default'/></interface>
<vsock model='virtio'><cid auto='yes'/></vsock></devices></domain>`

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func volume(t *testing.T, f *virttest.Fake, pool, name, capacity, data string) {
	t.Helper()
	_, err := f.CreateVolume(pool, "<volume><name>"+name+"</name><capacity unit='G'>"+capacity+"</capacity></volume>")
	must(t, err)
	must(t, f.UploadVolume(pool, name, strings.NewReader(data), uint64(len(data))))
}

func host(t *testing.T) *virttest.Fake {
	t.Helper()
	f := virttest.New()
	must(t, virt.EnsurePool(f, "default", "/img"))
	nvramDir := t.TempDir()
	_, err := f.DefineDomain(buildVM("ubuntu", nvramDir))
	must(t, err)
	volume(t, f, "default", "ubuntu.qcow2", "20", "12345")
	for _, name := range []string{"ubuntu_VARS.fd", "ubuntu_VARS.fd.provision-base.bak", "ubuntu_VARS.fd.clean-install.bak"} {
		must(t, os.WriteFile(filepath.Join(nvramDir, name), nil, 0o600))
	}
	must(t, f.CreateSnapshot("ubuntu", "native", ""))
	must(t, f.SetDomainDescription("ubuntu", "notes\n"+domxml.LUKSMarker))
	_, err = f.DefineDomain(vsockVM)
	must(t, err)
	return f
}

func noSleep(time.Duration) {}

func TestList(t *testing.T) {
	f := host(t)
	must(t, f.StartDomain("werft-a"))
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:02", IP: "fe80::1"})
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:02", IP: "192.168.122.9"})
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.5"})

	got, err := lifecycle.List(f)
	must(t, err)
	used, size := uint64(5), uint64(20<<30)
	want := []lifecycle.Entry{
		{Name: "ubuntu", State: "shut off", Used: &used, Size: &size, ResetPoints: 3, LUKS: true},
		{Name: "werft-a", State: "running", Address: "vsock/3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List =\n%+v\nwant\n%+v", got, want)
	}

	must(t, f.StartDomain("ubuntu"))
	got, err = lifecycle.List(f)
	must(t, err)
	if got[0].Address != "192.168.122.5" {
		t.Errorf("address of a domain without vsock = %q, want its IPv4 lease", got[0].Address)
	}
}

func TestListSkipsADomainThatGoesAway(t *testing.T) {
	f := host(t)
	f.FailOn("DomainState werft-a", failure.New(failure.NotFound, "gone"))
	got, err := lifecycle.List(f)
	must(t, err)
	if len(got) != 1 || got[0].Name != "ubuntu" {
		t.Errorf("List = %+v", got)
	}
	f.FailOn("Snapshots ubuntu", failure.New(failure.Failed, "broken"))
	if _, err := lifecycle.List(f); err == nil {
		t.Error("List hid a failed snapshot lookup")
	}
}

func TestStart(t *testing.T) {
	f := host(t)
	s, err := lifecycle.Start(f, "ubuntu")
	must(t, err)
	if s.AlreadyRunning || !s.LUKS || len(s.Leases) != 0 {
		t.Errorf("Start = %+v", s)
	}
	if st, _ := f.DomainState("ubuntu"); st != virt.StateRunning {
		t.Errorf("state = %v", st)
	}
	s, err = lifecycle.Start(f, "ubuntu")
	must(t, err)
	if !s.AlreadyRunning {
		t.Error("second Start did not see the running domain")
	}
	_, err = lifecycle.Start(f, "none")
	if !failure.Is(err, failure.NotFound) || !strings.Contains(err.Error(), "werft list") {
		t.Errorf("Start of an unknown domain = %v", err)
	}
}

func TestWaitAddress(t *testing.T) {
	f := host(t)
	must(t, f.StartDomain("ubuntu"))
	waited := time.Duration(0)
	sleep := func(d time.Duration) {
		waited += d
		if waited == 9*time.Second {
			f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.5"})
		}
	}
	if got := lifecycle.WaitAddress(f, "ubuntu", nil, sleep); got != "192.168.122.5" || waited != 9*time.Second {
		t.Errorf("WaitAddress = %q after %v", got, waited)
	}

	must(t, f.StartDomain("werft-a"))
	waited = 0
	if got := lifecycle.WaitAddress(f, "werft-a", nil, func(d time.Duration) { waited += d }); got != "" || waited < lifecycle.AddressTimeout {
		t.Errorf("WaitAddress without a lease = %q after %v; want none after the full wait, though the domain has a CID", got, waited)
	}
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:02", IP: "192.168.122.9"})
	if got := lifecycle.WaitAddress(f, "werft-a", nil, noSleep); got != "vsock/3" {
		t.Errorf("WaitAddress with a lease = %q, want the CID", got)
	}
}

// libvirt keeps a lease until it expires, also after the domain stopped:
// only a lease newer than the start shows that the guest is up.
func TestWaitAddressIgnoresALeaseFromBeforeTheStart(t *testing.T) {
	f := host(t)
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.5", Expiry: 100})
	s, err := lifecycle.Start(f, "ubuntu")
	must(t, err)
	if len(s.Leases) != 1 {
		t.Fatalf("Start kept leases %v, want the one from before the start", s.Leases)
	}
	waited := time.Duration(0)
	if got := lifecycle.WaitAddress(f, "ubuntu", s.Leases, func(d time.Duration) { waited += d }); got != "" || waited < lifecycle.AddressTimeout {
		t.Errorf("WaitAddress with only the old lease = %q after %v; want none after the full wait", got, waited)
	}
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.5", Expiry: 200})
	if got := lifecycle.WaitAddress(f, "ubuntu", s.Leases, noSleep); got != "192.168.122.5" {
		t.Errorf("WaitAddress with a renewed lease = %q", got)
	}
}

func TestStop(t *testing.T) {
	f := host(t)
	var progress bytes.Buffer
	got, err := lifecycle.Stop(f, "ubuntu", false, noSleep, &progress)
	must(t, err)
	if got != lifecycle.AlreadyShutOff || progress.Len() != 0 {
		t.Errorf("Stop of a shut-off domain = %v, %q", got, progress.String())
	}

	must(t, f.StartDomain("ubuntu"))
	got, err = lifecycle.Stop(f, "ubuntu", false, noSleep, &progress)
	must(t, err)
	if got != lifecycle.ShutDown || !strings.Contains(progress.String(), "ACPI shutdown sent to ubuntu") {
		t.Errorf("Stop = %v, %q", got, progress.String())
	}

	must(t, f.StartDomain("ubuntu"))
	got, err = lifecycle.Stop(f, "ubuntu", true, noSleep, &progress)
	must(t, err)
	if got != lifecycle.ForcedOff || f.Calls()[len(f.Calls())-1] != "DestroyDomain ubuntu" {
		t.Errorf("Stop --force = %v, calls %q", got, f.Calls())
	}

	_, err = lifecycle.Stop(f, "none", false, noSleep, &progress)
	if !failure.Is(err, failure.NotFound) {
		t.Errorf("Stop of an unknown domain = %v", err)
	}
}

func TestStopTimesOut(t *testing.T) {
	f := host(t)
	must(t, f.StartDomain("ubuntu"))
	f.IgnoreShutdown("ubuntu")
	waited := time.Duration(0)
	_, err := lifecycle.Stop(f, "ubuntu", false, func(d time.Duration) { waited += d }, &bytes.Buffer{})
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "werft stop --force ubuntu") {
		t.Errorf("Stop of a guest that ignores ACPI = %v", err)
	}
	if waited < lifecycle.ShutdownTimeout {
		t.Errorf("gave up after %v", waited)
	}
	if st, _ := f.DomainState("ubuntu"); st != virt.StateRunning {
		t.Errorf("state = %v; a timeout must not force the domain off", st)
	}
}
