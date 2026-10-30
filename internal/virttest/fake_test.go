package virttest_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

const vmXML = `<domain type='kvm'><name>vm1</name>
<os><type>hvm</type><loader type='pflash'>/code.fd</loader><nvram format='qcow2'>/pool/vm1-vars.qcow2</nvram></os>
<devices><vsock model='virtio'><cid auto='yes'/></vsock></devices></domain>`

func mustKind(t *testing.T, err error, want failure.Kind) {
	t.Helper()
	if got := failure.KindOf(err); got != want {
		t.Fatalf("error %v has kind %q, want %q", err, got, want)
	}
}

func newPool(t *testing.T, f *virttest.Fake) {
	t.Helper()
	if err := virt.EnsurePool(f, "pool", "/pool"); err != nil {
		t.Fatal(err)
	}
}

func TestDefineReservesTheName(t *testing.T) {
	f := virttest.New()
	d, err := f.DefineDomain(vmXML)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.DefineDomain(vmXML)
	mustKind(t, err, failure.Conflict)

	same := strings.Replace(vmXML, "<name>vm1</name>", "<name>vm1</name><uuid>"+d.UUID+"</uuid>", 1)
	if _, err := f.DefineDomain(same); err != nil {
		t.Errorf("redefine with the same UUID: %v", err)
	}
	other := strings.Replace(same, "<name>vm1</name>", "<name>vm2</name>", 1)
	_, err = f.DefineDomain(other)
	mustKind(t, err, failure.Conflict)
}

func TestLifecycle(t *testing.T) {
	f := virttest.New()
	if _, err := f.DefineDomain(vmXML); err != nil {
		t.Fatal(err)
	}
	mustKind(t, f.DestroyDomain("vm1"), failure.Conflict)
	if err := f.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	mustKind(t, f.StartDomain("vm1"), failure.Conflict)
	if s, _ := f.DomainState("vm1"); s != virt.StateRunning {
		t.Errorf("state = %v", s)
	}

	live, _ := f.DomainXML("vm1", false)
	x, _ := domxml.Parse(live)
	if cid, ok := domxml.VsockCID(x); !ok || cid != 3 {
		t.Errorf("live CID = %d, %v", cid, ok)
	}
	inactive, _ := f.DomainXML("vm1", true)
	x, _ = domxml.Parse(inactive)
	if _, ok := domxml.VsockCID(x); ok {
		t.Error("inactive XML has a CID")
	}

	if err := f.ShutdownDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.DomainState("vm1"); s != virt.StateShutOff {
		t.Errorf("state after shutdown = %v", s)
	}
	mustKind(t, f.UndefineDomain("vm1", 0), failure.Conflict)
	if err := f.UndefineDomain("vm1", virt.UndefineNVRAM); err != nil {
		t.Fatal(err)
	}
	_, err := f.Domain("vm1")
	mustKind(t, err, failure.NotFound)
}

func TestUndefineOfARunningDomainKeepsItUntilItStops(t *testing.T) {
	f := virttest.New()
	if _, err := f.DefineDomain(vmXML); err != nil {
		t.Fatal(err)
	}
	if err := f.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	if err := f.UndefineDomain("vm1", virt.UndefineKeepNVRAM); err != nil {
		t.Fatal(err)
	}
	if s, err := f.DomainState("vm1"); err != nil || s != virt.StateRunning {
		t.Errorf("state after undefine = %v, %v; want still running", s, err)
	}
	mustKind(t, f.UndefineDomain("vm1", virt.UndefineKeepNVRAM), failure.Conflict)
	if err := f.DestroyDomain("vm1"); err != nil {
		t.Fatalf("destroy of the transient domain: %v", err)
	}
	_, err := f.Domain("vm1")
	mustKind(t, err, failure.NotFound)
}

func TestUndefineNVRAMDeletesTheVolume(t *testing.T) {
	f := virttest.New()
	newPool(t, f)
	if _, err := f.CreateVolume("pool", "<volume><name>vm1-vars.qcow2</name></volume>"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DefineDomain(vmXML); err != nil {
		t.Fatal(err)
	}
	if err := f.UndefineDomain("vm1", virt.UndefineNVRAM); err != nil {
		t.Fatal(err)
	}
	_, err := f.VolumePath("pool", "vm1-vars.qcow2")
	mustKind(t, err, failure.NotFound)
}

func TestSnapshotsRestoreTheDefinition(t *testing.T) {
	f := virttest.New()
	if _, err := f.DefineDomain(vmXML); err != nil {
		t.Fatal(err)
	}
	if err := f.CreateSnapshot("vm1", "clean", "after install"); err != nil {
		t.Fatal(err)
	}
	mustKind(t, f.CreateSnapshot("vm1", "clean", ""), failure.Conflict)
	if err := f.SetDomainDescription("vm1", "changed"); err != nil {
		t.Fatal(err)
	}
	if err := f.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}

	sx, err := f.SnapshotXML("vm1", "clean")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sx, "<state>shutoff</state>") || !strings.Contains(sx, "/pool/vm1-vars.qcow2") {
		t.Errorf("snapshot XML lacks state or domain:\n%s", sx)
	}

	if err := f.RevertSnapshot("vm1", "clean"); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.DomainState("vm1"); s != virt.StateShutOff {
		t.Errorf("state after revert = %v", s)
	}
	if desc, _ := f.DomainDescription("vm1"); desc != "" {
		t.Errorf("description after revert = %q", desc)
	}

	mustKind(t, f.UndefineDomain("vm1", virt.UndefineKeepNVRAM), failure.Conflict)
	if err := f.DeleteSnapshot("vm1", "clean"); err != nil {
		t.Fatal(err)
	}
	mustKind(t, f.RevertSnapshot("vm1", "clean"), failure.NotFound)
	if names, _ := f.Snapshots("vm1"); len(names) != 0 {
		t.Errorf("snapshots after delete = %v", names)
	}
}

func TestMetadata(t *testing.T) {
	f := virttest.New()
	if _, err := f.DefineDomain(vmXML); err != nil {
		t.Fatal(err)
	}
	const uri = "https://werft.example/lease"
	_, err := f.DomainMetadata("vm1", uri)
	mustKind(t, err, failure.NotFound)
	if err := f.SetDomainMetadata("vm1", uri, "werft", "<lease/>"); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.DomainMetadata("vm1", uri); m != "<lease/>" {
		t.Errorf("metadata = %q", m)
	}
}

func TestVolumes(t *testing.T) {
	f := virttest.New()
	_, err := f.CreateVolume("pool", "<volume><name>a</name></volume>")
	mustKind(t, err, failure.NotFound)
	newPool(t, f)

	v, err := f.CreateVolume("pool", "<volume><name>a.raw</name><capacity unit='M'>1</capacity></volume>")
	if err != nil {
		t.Fatal(err)
	}
	if v.Path != "/pool/a.raw" {
		t.Errorf("path = %q", v.Path)
	}
	_, err = f.CreateVolume("pool", "<volume><name>a.raw</name></volume>")
	mustKind(t, err, failure.Conflict)

	if err := f.UploadVolume("pool", "a.raw", strings.NewReader("hello world"), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateVolumeFrom("pool", "<volume><name>b.qcow2</name></volume>", "pool", "a.raw"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.DownloadVolume("pool", "b.qcow2", &buf); err != nil || buf.String() != "hello" {
		t.Errorf("download = %q, %v", buf.String(), err)
	}
	info, _ := f.VolumeInfo("pool", "a.raw")
	if info != (virt.VolumeInfo{Capacity: 1 << 20, Allocation: 5}) {
		t.Errorf("info = %+v", info)
	}
	f.SetPoolCapacity("pool", 100)
	if p, _ := f.PoolInfo("pool"); p.Allocation != 10 || p.Available != 90 {
		t.Errorf("pool info = %+v", p)
	}
	if err := f.DeleteVolume("pool", "a.raw"); err != nil {
		t.Fatal(err)
	}
	mustKind(t, f.DeleteVolume("pool", "a.raw"), failure.NotFound)
}

func TestGuestDevices(t *testing.T) {
	f := virttest.New()
	if _, err := f.DefineDomain(vmXML); err != nil {
		t.Fatal(err)
	}
	mustKind(t, f.SendKey("vm1", []uint32{28}), failure.Conflict)
	if err := f.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	f.SetScreenshot("vm1", "image/png", []byte("png"))
	var buf bytes.Buffer
	if mime, err := f.Screenshot("vm1", &buf); err != nil || mime != "image/png" || buf.String() != "png" {
		t.Errorf("screenshot = %q %q %v", mime, buf.String(), err)
	}
	if err := f.SendKey("vm1", []uint32{42, 30}); err != nil {
		t.Fatal(err)
	}
	if got := f.SentKeys("vm1"); !reflect.DeepEqual(got, [][]uint32{{42, 30}}) {
		t.Errorf("sent keys = %v", got)
	}
	if err := f.AttachDevice("vm1", "<hostdev/>"); err != nil {
		t.Fatal(err)
	}
	if err := f.DetachDevice("vm1", "<hostdev/>"); err != nil {
		t.Fatal(err)
	}
	mustKind(t, f.DetachDevice("vm1", "<hostdev/>"), failure.NotFound)
}

func TestDHCPLeasesFilterByMAC(t *testing.T) {
	f := virttest.New()
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.10"})
	f.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:02", IP: "192.168.122.11"})
	ls, err := f.DHCPLeases("default", "52:54:00:00:00:02")
	if err != nil || len(ls) != 1 || ls[0].IP != "192.168.122.11" {
		t.Errorf("leases = %+v, %v", ls, err)
	}
}

func TestCallLogAndFailureInjection(t *testing.T) {
	f := virttest.New()
	boom := failure.New(failure.Timeout, "boom")
	f.FailOn("StartDomain vm1", boom)
	if _, err := f.DefineDomain(vmXML); err != nil {
		t.Fatal(err)
	}
	if err := f.StartDomain("vm1"); !errors.Is(err, boom) {
		t.Errorf("StartDomain = %v, want injected failure", err)
	}
	if s, _ := f.DomainState("vm1"); s != virt.StateShutOff {
		t.Error("a failed start changed the state")
	}
	f.FailOn("StartDomain vm1", nil)
	if err := f.StartDomain("vm1"); err != nil {
		t.Errorf("StartDomain after the failure was removed: %v", err)
	}
	f.FailOn("StartDomain vm", boom)
	if err := f.StartDomain("vm1"); errors.Is(err, boom) {
		t.Error("the failure for domain vm matched domain vm1")
	}
	want := []string{"DefineDomain vm1", "StartDomain vm1", "DomainState vm1", "StartDomain vm1", "StartDomain vm1"}
	if got := f.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestVolumeByPath(t *testing.T) {
	f := virttest.New()
	if err := f.DefinePool("p", "/pool"); err != nil {
		t.Fatal(err)
	}
	if err := f.StartPool("p"); err != nil {
		t.Fatal(err)
	}
	v, err := f.CreateVolume("p", "<volume><name>disk.qcow2</name><capacity>1</capacity></volume>")
	if err != nil {
		t.Fatal(err)
	}
	if pool, name, err := f.VolumeByPath(v.Path); err != nil || pool != "p" || name != "disk.qcow2" {
		t.Errorf("VolumeByPath = %q, %q, %v", pool, name, err)
	}
	if _, _, err := f.VolumeByPath("/elsewhere"); !failure.Is(err, failure.NotFound) {
		t.Errorf("VolumeByPath of a path in no pool = %v", err)
	}
}
