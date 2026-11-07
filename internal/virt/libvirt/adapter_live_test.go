//go:build live

package libvirt_test

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/virt"
)

// memTotal is MemTotal of /proc/meminfo in bytes, the memory libvirt
// reports for the host.
func memTotal(t *testing.T) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/meminfo")
	livetest.Must(t, err, "read /proc/meminfo")
	var kib uint64
	for line := range strings.SplitSeq(string(data), "\n") {
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &kib); err == nil {
			return kib << 10
		}
	}
	t.Fatal("/proc/meminfo has no MemTotal")
	return 0
}

func mustKind(t *testing.T, err error, want failure.Kind, what string) {
	t.Helper()
	if got := failure.KindOf(err); got != want {
		t.Fatalf("%s: error %v has kind %q, want %q", what, err, got, want)
	}
}

// eventually polls check until it holds or timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestAdapterBasics(t *testing.T) {
	h := livetest.Connect(t)

	v, err := h.LibVersion()
	livetest.Must(t, err, "libvirt version")
	if !v.AtLeast(virt.Version{Major: 11, Minor: 4}) {
		t.Fatalf("libvirt %v is below 11.4", v)
	}
	total, err := h.HostTotalMemory()
	livetest.Must(t, err, "host memory")
	if want := memTotal(t); total != want {
		t.Errorf("host memory = %d bytes, want MemTotal of /proc/meminfo, %d bytes", total, want)
	}

	g := livetest.NewGuest(t, h, "werft-test-fnd-adapter")
	data := g.Name + "-data.raw"
	livetest.DeleteVolumes(h, data)
	t.Cleanup(func() { livetest.DeleteVolumes(h, data) })

	checkLookup(t, h, g)
	checkDefineReservesTheName(t, h, g)
	checkMetadataAndDescription(t, h, g)
	checkSnapshots(t, h, g)
	checkVolumes(t, h, g, data)

	g.Boot()

	checkRunningDomain(t, h, g)
	checkDHCPLease(t, h, g)
	checkDeviceAttachAndDetach(t, h, g, data)
	checkShutdownAndUndefine(t, h, g)
}

func checkLookup(t *testing.T, h virt.Hypervisor, g *livetest.Guest) {
	t.Helper()
	d, err := h.Domain(g.Name)
	livetest.Must(t, err, "lookup")
	if len(d.UUID) != 36 {
		t.Errorf("UUID = %q", d.UUID)
	}
	pools, err := h.Pools()
	livetest.Must(t, err, "list pools")
	if !slices.Contains(pools, virt.Pool{Name: livetest.Pool, Path: livetest.PoolPath}) {
		t.Errorf("pools = %+v, want %s on %s", pools, livetest.Pool, livetest.PoolPath)
	}
	all, err := h.Domains()
	livetest.Must(t, err, "list domains")
	if !slices.Contains(all, d) {
		t.Errorf("Domains does not list %v", d)
	}
	_, err = h.Domain("werft-test-fnd-absent")
	mustKind(t, err, failure.NotFound, "lookup of a missing domain")
}

func checkDefineReservesTheName(t *testing.T, h virt.Hypervisor, g *livetest.Guest) {
	t.Helper()
	x, err := h.DomainXML(g.Name, true)
	livetest.Must(t, err, "inactive XML")
	d, err := domxml.Parse(x)
	livetest.Must(t, err, "parse")
	uuid := d.UUID
	d.UUID = ""
	again, err := d.Marshal()
	livetest.Must(t, err, "marshal")
	_, err = h.DefineDomain(again)
	mustKind(t, err, failure.Conflict, "define of a taken name")

	const other = "werft-test-fnd-uuid"
	livetest.RemoveDomain(t, h, other)
	t.Cleanup(func() { livetest.RemoveDomain(t, h, other) })
	d.Name, d.UUID = other, uuid
	sameUUID, err := d.Marshal()
	livetest.Must(t, err, "marshal")
	_, err = h.DefineDomain(sameUUID)
	mustKind(t, err, failure.Conflict, "define of a taken UUID under a new name")
}

func checkMetadataAndDescription(t *testing.T, h virt.Hypervisor, g *livetest.Guest) {
	t.Helper()
	const uri = "https://github.com/hk9890/werft/test"
	_, err := h.DomainMetadata(g.Name, uri)
	mustKind(t, err, failure.NotFound, "metadata before set")
	livetest.Must(t, h.SetDomainMetadata(g.Name, uri, "werft", `<lease owner="test"/>`), "set metadata")
	m, err := h.DomainMetadata(g.Name, uri)
	livetest.Must(t, err, "metadata")
	if !strings.Contains(m, `owner="test"`) {
		t.Errorf("metadata = %q", m)
	}
	livetest.Must(t, h.SetDomainMetadata(g.Name, uri, "werft", ""), "clear metadata")
	_, err = h.DomainMetadata(g.Name, uri)
	mustKind(t, err, failure.NotFound, "metadata after clear")

	desc, err := h.DomainDescription(g.Name)
	if err != nil || desc != "" {
		t.Errorf("description before set = %q, %v", desc, err)
	}
	livetest.Must(t, h.SetDomainDescription(g.Name, "one\ntwo"), "set description")
	if desc, _ := h.DomainDescription(g.Name); desc != "one\ntwo" {
		t.Errorf("description = %q", desc)
	}
	livetest.Must(t, h.SetDomainDescription(g.Name, ""), "clear description")
	if desc, _ := h.DomainDescription(g.Name); desc != "" {
		t.Errorf("description after clear = %q", desc)
	}
}

func checkSnapshots(t *testing.T, h virt.Hypervisor, g *livetest.Guest) {
	t.Helper()
	livetest.Must(t, h.CreateSnapshot(g.Name, "s1", "first"), "create snapshot")
	mustKind(t, h.CreateSnapshot(g.Name, "s1", ""), failure.Conflict, "second snapshot of one name")
	names, err := h.Snapshots(g.Name)
	livetest.Must(t, err, "list snapshots")
	if !slices.Equal(names, []string{"s1"}) {
		t.Errorf("snapshots = %v", names)
	}
	x, err := h.SnapshotXML(g.Name, "s1")
	livetest.Must(t, err, "snapshot XML")
	if !strings.Contains(x, "<description>first</description>") {
		t.Errorf("snapshot XML lacks the description:\n%s", x)
	}
	livetest.Must(t, h.RevertSnapshot(g.Name, "s1"), "revert")
	_, err = h.SnapshotXML(g.Name, "nope")
	mustKind(t, err, failure.NotFound, "missing snapshot")
	livetest.Must(t, h.DeleteSnapshot(g.Name, "s1"), "delete snapshot")
	if names, _ := h.Snapshots(g.Name); len(names) != 0 {
		t.Errorf("snapshots after delete = %v", names)
	}
}

func checkVolumes(t *testing.T, h virt.Hypervisor, g *livetest.Guest, data string) {
	t.Helper()
	info, err := h.PoolInfo(livetest.Pool)
	livetest.Must(t, err, "pool info")
	if !info.Active || info.Capacity == 0 {
		t.Errorf("pool info = %+v", info)
	}
	_, err = h.CreateVolume(livetest.Pool, fmt.Sprintf("<volume><name>%s</name><capacity unit='M'>1</capacity><target><format type='raw'/></target></volume>", data))
	livetest.Must(t, err, "create volume")
	livetest.Must(t, h.UploadVolume(livetest.Pool, data, strings.NewReader("hello"), 5), "upload")
	var buf bytes.Buffer
	livetest.Must(t, h.DownloadVolume(livetest.Pool, data, &buf), "download")
	if buf.Len() != 1<<20 || !bytes.HasPrefix(buf.Bytes(), []byte("hello")) {
		t.Errorf("download = %d bytes starting %q", buf.Len(), buf.Bytes()[:min(5, buf.Len())])
	}
	vi, err := h.VolumeInfo(livetest.Pool, data)
	livetest.Must(t, err, "volume info")
	if vi.Capacity != 1<<20 {
		t.Errorf("volume info = %+v", vi)
	}
	_, err = h.VolumeInfo(livetest.Pool, "werft-test-fnd-absent.raw")
	mustKind(t, err, failure.NotFound, "missing volume")
}

func checkRunningDomain(t *testing.T, h virt.Hypervisor, g *livetest.Guest) {
	t.Helper()
	if s, err := h.DomainState(g.Name); err != nil || s != virt.StateRunning {
		t.Errorf("state = %v, %v", s, err)
	}
	mustKind(t, h.StartDomain(g.Name), failure.Conflict, "start of a running domain")
	var img bytes.Buffer
	mime, err := h.Screenshot(g.Name, &img)
	livetest.Must(t, err, "screenshot")
	if mime == "" || img.Len() == 0 {
		t.Errorf("screenshot = %q, %d bytes", mime, img.Len())
	}
	const keyLeftShift = 42
	livetest.Must(t, h.SendKey(g.Name, []uint32{keyLeftShift}), "send key")
}

func checkDHCPLease(t *testing.T, h virt.Hypervisor, g *livetest.Guest) {
	t.Helper()
	x, err := h.DomainXML(g.Name, true)
	livetest.Must(t, err, "inactive XML")
	d, err := domxml.Parse(x)
	livetest.Must(t, err, "parse")
	ifaces := domxml.Interfaces(d)
	if len(ifaces) != 1 || ifaces[0].MAC == "" {
		t.Fatalf("Interfaces = %+v", ifaces)
	}
	mac := ifaces[0].MAC
	var leases []virt.DHCPLease
	eventually(t, time.Minute, "DHCP lease for "+mac, func() bool {
		leases, err = h.DHCPLeases("default", mac)
		livetest.Must(t, err, "DHCP leases")
		return len(leases) > 0
	})
	if out := g.Run("ip -4 -o addr"); !strings.Contains(out, leases[0].IP+"/") {
		t.Errorf("guest addresses lack the lease IP %s:\n%s", leases[0].IP, out)
	}
}

func checkDeviceAttachAndDetach(t *testing.T, h virt.Hypervisor, g *livetest.Guest, data string) {
	t.Helper()
	path, err := h.VolumePath(livetest.Pool, data)
	livetest.Must(t, err, "volume path")
	dev := fmt.Sprintf(`<disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='%s'/><target dev='vdb' bus='virtio'/></disk>`, path)
	livetest.Must(t, h.AttachDevice(g.Name, dev), "attach")
	eventually(t, 30*time.Second, "vdb in the guest", func() bool {
		_, err := g.SSH("test -b /dev/vdb")
		return err == nil
	})
	if out := g.Run("head -c 5 /dev/vdb"); out != "hello" {
		t.Errorf("vdb starts %q", out)
	}
	livetest.Must(t, h.DetachDevice(g.Name, dev), "detach")
	eventually(t, 30*time.Second, "vdb gone from the live XML", func() bool {
		x, err := h.DomainXML(g.Name, false)
		livetest.Must(t, err, "live XML")
		return !strings.Contains(x, "dev='vdb'") && !strings.Contains(x, `dev="vdb"`)
	})
}

// checkShutdownAndUndefine also proves what seal relies on: a description
// set while the domain runs (detect-guest) stays through the poweroff and
// reaches the definition of a snapshot taken after it.
func checkShutdownAndUndefine(t *testing.T, h virt.Hypervisor, g *livetest.Guest) {
	t.Helper()
	line := domxml.SystemdLine(255)
	x, err := h.DomainXML(g.Name, true)
	livetest.Must(t, err, "inactive XML")
	if strings.Contains(x, line) {
		t.Fatalf("the definition has the systemd line before the set:\n%s", x)
	}
	livetest.Must(t, h.SetDomainDescription(g.Name, line), "set description while running")
	livetest.Must(t, h.ShutdownDomain(g.Name), "shutdown")
	g.WaitShutOff(2 * time.Minute)
	livetest.Must(t, h.CreateSnapshot(g.Name, "s2", ""), "snapshot after the poweroff")
	x, err = h.SnapshotXML(g.Name, "s2")
	livetest.Must(t, err, "snapshot XML")
	if !strings.Contains(x, line) {
		t.Errorf("the snapshot lacks the description set while the domain ran:\n%s", x)
	}
	livetest.Must(t, h.DeleteSnapshot(g.Name, "s2"), "delete snapshot")
	mustKind(t, h.DestroyDomain(g.Name), failure.Conflict, "destroy of a shut-off domain")
	livetest.Must(t, h.UndefineDomain(g.Name, virt.UndefineNVRAM), "undefine")
	_, err = h.Domain(g.Name)
	mustKind(t, err, failure.NotFound, "lookup after undefine")
}
