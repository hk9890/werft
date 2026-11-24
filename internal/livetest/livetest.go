//go:build live

package livetest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// Image255 is the path of a cloud image with systemd 255, $WERFT_TEST_IMAGE_255
// or the cached Ubuntu 24.04 minimal image. The test skips when it is missing.
func Image255(t *testing.T) string {
	t.Helper()
	image := os.Getenv("WERFT_TEST_IMAGE_255")
	if image == "" {
		image = filepath.Join(os.Getenv("HOME"), ".cache/werft/images/ubuntu-24.04-minimal-cloudimg-amd64.img")
	}
	if _, err := os.Stat(image); err != nil {
		t.Skipf("no systemd 255 test image at %s (set WERFT_TEST_IMAGE_255)", image)
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
// later runs reuse the volume. The volume has the name of the image file. A
// marker file next to the lock records a finished upload, so a volume that
// an interrupted run left half written is uploaded again. The lock keeps
// runs in parallel from using a volume while one of them uploads it.
func BaseImage(t *testing.T, h virt.Hypervisor, image string) string {
	t.Helper()
	cache := filepath.Join(os.Getenv("HOME"), ".cache/werft")
	Must(t, os.MkdirAll(cache, 0o755), "create "+cache)
	lock, err := os.OpenFile(filepath.Join(cache, "base-image.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	Must(t, err, "open base image lock")
	defer func() { _ = lock.Close() }()
	Must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX), "lock base image")

	st, err := os.Stat(image)
	Must(t, err, "stat "+image)
	name := filepath.Base(image)
	marker := filepath.Join(cache, Pool+"-"+name+".uploaded")
	size := strconv.FormatInt(st.Size(), 10)
	if done, err := os.ReadFile(marker); err == nil && string(done) == size {
		if path, err := h.VolumePath(Pool, name); err == nil {
			return path
		}
	}
	_ = os.Remove(marker)
	_ = h.DeleteVolume(Pool, name)
	Upload(t, h, name, image)
	Must(t, os.WriteFile(marker, []byte(size), 0o644), "write "+marker)
	path, err := h.VolumePath(Pool, name)
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
// metadata, and ignores a domain that is gone. It refuses a name without
// DomainPrefix, so it never touches a domain a live test did not create.
func RemoveDomain(t *testing.T, h virt.Hypervisor, name string) {
	t.Helper()
	if !strings.HasPrefix(name, DomainPrefix) {
		t.Fatalf("live test domain %q does not start with %q", name, DomainPrefix)
	}
	_ = h.DestroyDomain(name)
	_ = h.UndefineDomain(name, virt.UndefineSnapshots|virt.UndefineKeepNVRAM)
}

// DeleteVolumes ignores a volume that is gone. It refreshes the pool first,
// so it also finds a file QEMU created, such as a console log.
func DeleteVolumes(h virt.Hypervisor, names ...string) {
	_ = refresh(h)
	for _, name := range names {
		_ = h.DeleteVolume(Pool, name)
	}
}

// ConsoleTail is the last 60 lines of a serial log volume. It reads through
// the pool, which needs no root.
func ConsoleTail(h virt.Hypervisor, volume string) string {
	_ = refresh(h)
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

// refresh makes the pool list files QEMU created, such as a console log.
// libvirt refuses it while another run builds a volume in the pool, so it
// tries again for some seconds.
func refresh(h virt.Hypervisor) error {
	var err error
	for range 15 {
		if err = h.RefreshPool(Pool); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return err
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
// NVRAM, reached as root over vsock SSH, or over TCP at its DHCP address when
// NewTCPGuest made it.
type Guest struct {
	T    *testing.T
	H    virt.Hypervisor
	Name string
	Key  Key
	CID  uint32
	IP   string
	tcp  bool

	networkFileWritten bool
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
	g := &Guest{T: t, H: h, Name: name}
	g.define(Image(t))
	return g
}

// NewTCPGuest is NewGuest for a systemd 255 image. The guest has no vsock
// device, and its only credential is tmpfiles.extra.
func NewTCPGuest(t *testing.T, h virt.Hypervisor, name string) *Guest {
	t.Helper()
	g := &Guest{T: t, H: h, Name: name, tcp: true}
	g.define(Image255(t))
	return g
}

func (g *Guest) define(image string) {
	t, h, name := g.T, g.H, g.Name
	t.Helper()
	RemoveDomain(t, h, name)
	EnsurePool(t, h)
	base := BaseImage(t, h, image)
	DeleteVolumes(h, g.volumes()...)
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("WERFT_TEST_KEEP") != "" {
			t.Logf("kept %s for inspection; the next run removes it", name)
			return
		}
		RemoveDomain(t, h, name)
		DeleteVolumes(h, g.volumes()...)
	})

	disk := Overlay(t, h, g.DiskVolume(), base)
	vars := QCOW2Vars(t, h, g.VarsVolume())
	g.Key = NewKey(t)
	credentials, vsock := vsockCredentials(g.Key), vsockDevice
	if g.tcp {
		credentials, vsock = tcpCredentials(g.Key), ""
	}
	_, err := h.DefineDomain(g.domainXML(disk, vars, credentials, vsock))
	Must(t, err, "define "+name)
}

const vsockDevice = "    <vsock model='virtio'><cid auto='yes'/></vsock>\n"

func vsockCredentials(key Key) string {
	return fmt.Sprintf(`      <entry>io.systemd.credential:ssh.ephemeral-authorized_keys-all=%s</entry>
      <entry>io.systemd.credential.binary:systemd.unit-dropin.sshd-vsock@.service=%s</entry>
      <entry>io.systemd.credential.binary:network.network.50-werft-test=%s</entry>
      <entry>io.systemd.credential.binary:systemd.unit-dropin.systemd-networkd.service=%s</entry>
`, key.Public, hostKeyDropin, dhcpNetwork, networkdDropin)
}

// tcpCredentials writes the root key, a networkd DHCP file and the units of
// seal.sh that make the SSH host keys through tmpfiles.extra, the credential
// that systemd 255 imports. An OEM string is in the argv of QEMU, so it holds
// no private key.
func tcpCredentials(key Key) string {
	encode := base64.StdEncoding.EncodeToString
	hostKeys := "[Unit]\nDescription=SSH host keys of a werft stock image\n[Service]\nType=oneshot\nExecStart=/usr/bin/ssh-keygen -A\n"
	sshDropin := "[Unit]\nWants=werft-ssh-host-keys.service\nAfter=werft-ssh-host-keys.service\n"
	lines := strings.Join([]string{
		"d /root/.ssh 0700 root root -",
		"f /root/.ssh/authorized_keys 0600 root root -",
		"w+~ /root/.ssh/authorized_keys - - - - " + encode([]byte("\n"+key.Public+"\n")),
		"f~ /etc/systemd/system/werft-ssh-host-keys.service 0644 root root - " + encode([]byte(hostKeys)),
		"d /etc/systemd/system/ssh.service.d 0755 root root -",
		"f~ /etc/systemd/system/ssh.service.d/werft-host-keys.conf 0644 root root - " + encode([]byte(sshDropin)),
		"f~ /etc/systemd/network/50-werft-test.network 0644 root root - " + dhcpNetwork,
	}, "\n") + "\n"
	return "      <entry>io.systemd.credential.binary:tmpfiles.extra=" + encode([]byte(lines)) + "</entry>\n"
}

// domainXML has a VNC display without a listener, for screenshots, and more
// PCIe ports than its devices take, for hot-plugged ones.
func (g *Guest) domainXML(disk, vars, credentials, vsock string) string {
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
%s    </oemStrings>
  </sysinfo>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <controller type='pci' index='0' model='pcie-root'/>
%s    <interface type='network'><source network='default'/><model type='virtio'/></interface>
%s    <serial type='file'><source path='%s'/></serial>
    <graphics type='vnc'><listen type='none'/></graphics>
    <video><model type='virtio'/></video>
  </devices>
</domain>`, g.Name, ovmfCode, vars, credentials, disk,
		strings.Repeat("    <controller type='pci' model='pcie-root-port'/>\n", 10), vsock, PoolPath+"/"+g.ConsoleVolume())
}

// Boot starts the domain and waits until SSH answers, over vsock or at the
// DHCP address of a TCP guest.
func (g *Guest) Boot() {
	g.T.Helper()
	if g.tcp && !g.networkFileWritten {
		g.writeNetworkFile()
	}
	Must(g.T, g.H.StartDomain(g.Name), "start "+g.Name)
	if !g.tcp {
		g.CID = g.liveCID()
	}
	deadline := time.Now().Add(bootTimeout)
	for {
		if g.tcp {
			if _, ips := virt.DomainAddresses(g.H, g.Name); len(ips) > 0 {
				g.IP = ips[0]
			}
		}
		out, err := g.SSH("true")
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			g.T.Fatalf("no SSH to %s within %v: %v\n%s\nconsole:\n%s", g.host(), bootTimeout, err, out, ConsoleTail(g.H, g.ConsoleVolume()))
		}
		time.Sleep(5 * time.Second)
	}
}

// writeNetworkFile boots once and shuts down cleanly. On systemd 255,
// systemd-networkd starts before systemd-tmpfiles-setup, so the network file
// and the host-key units from tmpfiles.extra take effect only at the next boot. logind handles the
// ACPI request, and logind starts after tmpfiles ran.
func (g *Guest) writeNetworkFile() {
	g.T.Helper()
	Must(g.T, g.H.StartDomain(g.Name), "start "+g.Name)
	deadline := time.Now().Add(bootTimeout)
	for {
		state, err := g.H.DomainState(g.Name)
		Must(g.T, err, "domain state")
		if state == virt.StateShutOff {
			g.networkFileWritten = true
			return
		}
		if time.Now().After(deadline) {
			g.T.Fatalf("%s did not shut down after its first boot within %v\nconsole:\n%s", g.Name, bootTimeout, ConsoleTail(g.H, g.ConsoleVolume()))
		}
		_ = g.H.ShutdownDomain(g.Name)
		time.Sleep(3 * time.Second)
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

func (g *Guest) host() string {
	if g.tcp {
		return g.IP
	}
	return fmt.Sprintf("vsock/%d", g.CID)
}

// SSH runs cmd as root, with no user ssh_config and no known hosts.
func (g *Guest) SSH(cmd string) ([]byte, error) {
	args := []string{"-F", "/dev/null",
		"-i", g.Key.Private,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	if g.tcp {
		args = append(args, "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3")
	} else {
		args = append(args, "-o", "ProxyCommand="+sshProxy+" %h %p", "-o", "ProxyUseFdpass=yes")
	}
	return exec.Command("ssh", append(args, "root@"+g.host(), cmd)...).CombinedOutput()
}
