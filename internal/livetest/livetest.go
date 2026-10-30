//go:build live

package livetest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virt/libvirt"
)

const (
	Pool         = "werft-test"
	PoolPath     = "/var/lib/libvirt/images/werft-test"
	DomainPrefix = "werft-test-"
	baseVolume   = "ubuntu-26.04-minimal.img"
	ovmfCode     = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	ovmfVars     = "/usr/share/OVMF/OVMF_VARS_4M.fd"
	sshProxy     = "/usr/lib/systemd/systemd-ssh-proxy"
	bootTimeout  = 3 * time.Minute
)

// Image is the path of the cloud image, $WERFT_TEST_IMAGE or the cached
// Ubuntu minimal image. The test skips when it is missing.
func Image(t *testing.T) string {
	t.Helper()
	image := os.Getenv("WERFT_TEST_IMAGE")
	if image == "" {
		image = filepath.Join(os.Getenv("HOME"), ".cache/werft/images/ubuntu-26.04-minimal-cloudimg-amd64.img")
	}
	if _, err := os.Stat(image); err != nil {
		t.Skipf("no test image at %s (set WERFT_TEST_IMAGE)", image)
	}
	return image
}

// Connect dials qemu:///system and closes the connection at the end of the
// test. The test skips when libvirt does not answer.
func Connect(t *testing.T) *libvirt.Adapter {
	t.Helper()
	h, err := libvirt.Dial()
	if err != nil {
		t.Skipf("no libvirt: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func Must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func EnsurePool(t *testing.T, h virt.Hypervisor) {
	t.Helper()
	Must(t, virt.EnsurePool(h, Pool, PoolPath), "ensure pool "+Pool)
}

// BaseImage uploads image into the pool once and returns the volume path;
// later runs reuse the volume.
func BaseImage(t *testing.T, h virt.Hypervisor, image string) string {
	t.Helper()
	if path, err := h.VolumePath(Pool, baseVolume); err == nil {
		return path
	}
	Upload(t, h, baseVolume, image)
	Must(t, h.RefreshPool(Pool), "refresh pool")
	path, err := h.VolumePath(Pool, baseVolume)
	Must(t, err, "base volume path")
	return path
}

// Upload copies a host file into a new raw volume.
func Upload(t *testing.T, h virt.Hypervisor, name, file string) {
	t.Helper()
	f, err := os.Open(file)
	Must(t, err, "open "+file)
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	Must(t, err, "stat "+file)
	xml := fmt.Sprintf("<volume><name>%s</name><capacity unit='bytes'>%d</capacity><target><format type='raw'/></target></volume>", name, st.Size())
	_, err = h.CreateVolume(Pool, xml)
	Must(t, err, "create volume "+name)
	Must(t, h.UploadVolume(Pool, name, f, uint64(st.Size())), "upload "+name)
}

// Overlay creates a 10 GiB qcow2 volume backed by base and returns its path.
func Overlay(t *testing.T, h virt.Hypervisor, name, base string) string {
	t.Helper()
	xml := fmt.Sprintf(`<volume><name>%s</name><capacity unit='G'>10</capacity>
<target><format type='qcow2'/></target>
<backingStore><path>%s</path><format type='qcow2'/></backingStore></volume>`, name, base)
	v, err := h.CreateVolume(Pool, xml)
	Must(t, err, "create overlay "+name)
	return v.Path
}

// QCOW2Vars converts the raw OVMF vars template into a qcow2 volume, the
// format that lets libvirt snapshot the NVRAM, and returns its path.
func QCOW2Vars(t *testing.T, h virt.Hypervisor, name string) string {
	t.Helper()
	raw := name + ".raw"
	Upload(t, h, raw, ovmfVars)
	xml := fmt.Sprintf("<volume><name>%s</name><capacity>0</capacity><target><format type='qcow2'/></target></volume>", name)
	v, err := h.CreateVolumeFrom(Pool, xml, Pool, raw)
	Must(t, err, "convert vars to qcow2")
	Must(t, h.DeleteVolume(Pool, raw), "delete raw vars")
	return v.Path
}

type Key struct{ Private, Public string }

func NewKey(t *testing.T) Key {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "werft-test", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(path + ".pub")
	Must(t, err, "read public key")
	return Key{Private: path, Public: string(bytes.TrimSpace(pub))}
}

// RemoveDomain forces off and undefines a domain, with its snapshot
// metadata, and ignores a domain that is gone.
func RemoveDomain(h virt.Hypervisor, name string) {
	_ = h.DestroyDomain(name)
	_ = h.UndefineDomain(name, virt.UndefineSnapshots|virt.UndefineKeepNVRAM)
}

// DeleteVolumes ignores a volume that is gone. It refreshes the pool first,
// so it also finds a file QEMU created, such as a console log.
func DeleteVolumes(h virt.Hypervisor, names ...string) {
	_ = h.RefreshPool(Pool)
	for _, name := range names {
		_ = h.DeleteVolume(Pool, name)
	}
}

// ConsoleTail is the last 60 lines of a serial log volume. It reads through
// the pool, which needs no root.
func ConsoleTail(h virt.Hypervisor, volume string) string {
	_ = h.RefreshPool(Pool)
	var buf bytes.Buffer
	if err := h.DownloadVolume(Pool, volume, &buf); err != nil {
		return err.Error()
	}
	lines := strings.Split(buf.String(), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	return strings.Join(lines, "\n")
}

// hostKeyDropin makes sshd create its host keys. A cloud image ships without
// them and leaves that to cloud-init, which has no datasource here.
var hostKeyDropin = base64.StdEncoding.EncodeToString([]byte("[Service]\nExecStartPre=/usr/bin/ssh-keygen -A\n"))

// dhcpNetwork makes systemd-networkd run DHCP on the NIC, so the domain gets
// a DHCP lease. cloud-init would configure it, and the image leaves
// systemd-network-generator, which turns the credential into a network
// file, disabled; networkdDropin pulls it in.
var (
	dhcpNetwork    = base64.StdEncoding.EncodeToString([]byte("[Match]\nType=ether\n[Network]\nDHCP=yes\n"))
	networkdDropin = base64.StdEncoding.EncodeToString([]byte("[Unit]\nWants=systemd-network-generator.service\nAfter=systemd-network-generator.service\n"))
)

// Guest is a scratch UEFI domain on an overlay of the base image, with qcow2
// NVRAM, reached as root over vsock SSH.
type Guest struct {
	T    *testing.T
	H    virt.Hypervisor
	Name string
	Key  Key
	CID  uint32
}

func (g *Guest) DiskVolume() string    { return g.Name + "-disk.qcow2" }
func (g *Guest) VarsVolume() string    { return g.Name + "-vars.qcow2" }
func (g *Guest) ConsoleVolume() string { return g.Name + "-console.log" }

func (g *Guest) volumes() []string {
	return []string{g.DiskVolume(), g.VarsVolume() + ".raw", g.VarsVolume(), g.ConsoleVolume()}
}

// NewGuest defines the domain name, shut off. It first removes what an
// earlier run left under that name, and removes the domain and its volumes
// when the test ends, unless the test failed and WERFT_TEST_KEEP is set.
func NewGuest(t *testing.T, h virt.Hypervisor, name string) *Guest {
	t.Helper()
	if !strings.HasPrefix(name, DomainPrefix) {
		t.Fatalf("live test domain %q does not start with %q", name, DomainPrefix)
	}
	g := &Guest{T: t, H: h, Name: name}
	EnsurePool(t, h)
	base := BaseImage(t, h, Image(t))
	RemoveDomain(h, name)
	DeleteVolumes(h, g.volumes()...)
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("WERFT_TEST_KEEP") != "" {
			t.Logf("kept %s for inspection; the next run removes it", name)
			return
		}
		RemoveDomain(h, name)
		DeleteVolumes(h, g.volumes()...)
	})

	disk := Overlay(t, h, g.DiskVolume(), base)
	vars := QCOW2Vars(t, h, g.VarsVolume())
	g.Key = NewKey(t)
	_, err := h.DefineDomain(g.domainXML(disk, vars))
	Must(t, err, "define "+name)
	return g
}

// domainXML has a VNC display without a listener, for screenshots, and more
// PCIe ports than its devices take, for hot-plugged ones.
func (g *Guest) domainXML(disk, vars string) string {
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <memory unit='MiB'>1024</memory>
  <vcpu>2</vcpu>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <loader readonly='yes' type='pflash' format='raw'>%s</loader>
    <nvram format='qcow2'>%s</nvram>
    <smbios mode='sysinfo'/>
  </os>
  <features><acpi/></features>
  <sysinfo type='smbios'>
    <oemStrings>
      <entry>io.systemd.credential:ssh.ephemeral-authorized_keys-all=%s</entry>
      <entry>io.systemd.credential.binary:systemd.unit-dropin.sshd-vsock@.service=%s</entry>
      <entry>io.systemd.credential.binary:network.network.50-werft-test=%s</entry>
      <entry>io.systemd.credential.binary:systemd.unit-dropin.systemd-networkd.service=%s</entry>
    </oemStrings>
  </sysinfo>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <controller type='pci' index='0' model='pcie-root'/>
%s    <interface type='network'><source network='default'/><model type='virtio'/></interface>
    <vsock model='virtio'><cid auto='yes'/></vsock>
    <serial type='file'><source path='%s'/></serial>
    <graphics type='vnc'><listen type='none'/></graphics>
    <video><model type='virtio'/></video>
  </devices>
</domain>`, g.Name, ovmfCode, vars, g.Key.Public, hostKeyDropin, dhcpNetwork, networkdDropin, disk,
		strings.Repeat("    <controller type='pci' model='pcie-root-port'/>\n", 10), PoolPath+"/"+g.ConsoleVolume())
}

// Boot starts the domain and waits until SSH answers over vsock.
func (g *Guest) Boot() {
	g.T.Helper()
	Must(g.T, g.H.StartDomain(g.Name), "start "+g.Name)
	g.CID = g.liveCID()
	deadline := time.Now().Add(bootTimeout)
	for {
		out, err := g.SSH("true")
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			g.T.Fatalf("no SSH over vsock/%d within %v: %v\n%s\nconsole:\n%s", g.CID, bootTimeout, err, out, ConsoleTail(g.H, g.ConsoleVolume()))
		}
		time.Sleep(5 * time.Second)
	}
}

func (g *Guest) liveCID() uint32 {
	g.T.Helper()
	desc, err := g.H.DomainXML(g.Name, false)
	Must(g.T, err, "live domain XML")
	d, err := domxml.Parse(desc)
	Must(g.T, err, "parse live domain XML")
	cid, ok := domxml.VsockCID(d)
	if !ok {
		g.T.Fatal("live domain XML has no vsock CID")
	}
	return cid
}

// Run runs a guest command and fails the test when it fails.
func (g *Guest) Run(cmd string) string {
	g.T.Helper()
	out, err := g.SSH(cmd)
	if err != nil {
		g.T.Fatalf("guest command %q: %v\n%s", cmd, err, out)
	}
	return string(out)
}

// Poweroff powers off from inside the guest and waits until the domain is
// shut off.
func (g *Guest) Poweroff() {
	g.T.Helper()
	_, _ = g.SSH("systemctl poweroff")
	g.WaitShutOff(2 * time.Minute)
}

func (g *Guest) WaitShutOff(timeout time.Duration) {
	g.T.Helper()
	deadline := time.Now().Add(timeout)
	for {
		state, err := g.H.DomainState(g.Name)
		Must(g.T, err, "domain state")
		if state == virt.StateShutOff {
			return
		}
		if time.Now().After(deadline) {
			g.T.Fatalf("%s did not shut off within %v", g.Name, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// SSH runs cmd as root over vsock, with no user ssh_config and no known hosts.
func (g *Guest) SSH(cmd string) ([]byte, error) {
	c := exec.Command("ssh", "-F", "/dev/null",
		"-i", g.Key.Private,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ProxyCommand="+sshProxy+" %h %p",
		"-o", "ProxyUseFdpass=yes",
		fmt.Sprintf("root@vsock/%d", g.CID), cmd)
	return c.CombinedOutput()
}
