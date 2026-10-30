//go:build live

package libvirt

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"libvirt.org/go/libvirtxml"
)

// The reset-point design (docs/DESIGN.md, "Reset points") rests on one claim:
// a libvirt internal snapshot of a shut-off UEFI domain whose NVRAM is qcow2
// restores the firmware variables together with the disk. This test proves it
// on a scratch domain, with the same overlay, credential and vsock mechanics a
// lease uses.

const (
	testPool      = "werft-test"
	testPoolPath  = "/var/lib/libvirt/images/werft-test"
	testDomain    = "werft-test-uefi"
	baseVolume    = "ubuntu-26.04-minimal.img"
	diskVolume    = testDomain + "-disk.qcow2"
	varsRawVolume = testDomain + "-vars.raw"
	varsVolume    = testDomain + "-vars.qcow2"
	ovmfCode      = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	ovmfVars      = "/usr/share/OVMF/OVMF_VARS_4M.fd"
	sshProxy      = "/usr/lib/systemd/systemd-ssh-proxy"
	// NV | BS | RT, then the value "werft".
	efiVarValue = `\x07\x00\x00\x00werft`
	efiVarPath  = "/sys/firmware/efi/efivars/WerftResetTest-3f1b2c4d-5e6f-4a1b-9c8d-7e6f5a4b3c2d"
	diskMarker  = "/root/werft-reset-marker"
	consoleLog  = testDomain + "-console.log"
)

func TestUEFIResetPointRestoresNVRAMAndDisk(t *testing.T) {
	image := os.Getenv("WERFT_TEST_IMAGE")
	if image == "" {
		image = filepath.Join(os.Getenv("HOME"), ".cache/werft/images/ubuntu-26.04-minimal-cloudimg-amd64.img")
	}
	if _, err := os.Stat(image); err != nil {
		t.Skipf("no test image at %s (set WERFT_TEST_IMAGE)", image)
	}
	l, err := Dial()
	if err != nil {
		t.Skipf("no libvirt: %v", err)
	}
	t.Cleanup(func() { _ = l.Disconnect() })

	pool := ensurePool(t, l)
	removeDomain(l)
	deleteVolumes(l, pool, diskVolume, varsRawVolume, varsVolume, consoleLog)
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("WERFT_TEST_KEEP") != "" {
			t.Logf("kept %s for inspection; the next run removes it", testDomain)
			return
		}
		removeDomain(l)
		deleteVolumes(l, pool, diskVolume, varsRawVolume, varsVolume, consoleLog)
	})

	base := ensureBaseVolume(t, l, pool, image)
	disk := createOverlay(t, l, pool, base)
	vars := createQcow2Vars(t, l, pool)
	key := newSSHKey(t)

	dom, err := l.DomainDefineXMLFlags(domainXML(disk, vars, key.public), 0)
	must(t, err, "define domain")

	g := guest{t: t, l: l, dom: dom, key: key.private}

	g.boot()
	g.poweroff()
	snap, err := l.DomainSnapshotCreateXML(dom, "<domainsnapshot><name>base</name></domainsnapshot>", 0)
	must(t, err, "create reset point")
	assertSnapshotCoversNVRAM(t, l, snap)

	g.boot()
	g.run(fmt.Sprintf("printf '%s' > %s && echo changed > %s && sync", efiVarValue, efiVarPath, diskMarker))
	g.poweroff()

	// Control: without a revert both changes survive a reboot, so their absence
	// after the revert means something.
	g.boot()
	g.run(fmt.Sprintf("test -e %s && test -e %s", efiVarPath, diskMarker))
	g.poweroff()

	must(t, l.DomainRevertToSnapshot(snap, 0), "revert to reset point")

	g.boot()
	g.run(fmt.Sprintf("test ! -e %s", efiVarPath))
	g.run(fmt.Sprintf("test ! -e %s", diskMarker))
	g.poweroff()
}

type guest struct {
	t   *testing.T
	l   *golibvirt.Libvirt
	dom golibvirt.Domain
	key string
	cid uint
}

func (g *guest) boot() {
	g.t.Helper()
	must(g.t, g.l.DomainCreate(g.dom), "start domain")
	g.cid = liveCID(g.t, g.l, g.dom)
	deadline := time.Now().Add(90 * time.Second)
	for {
		out, err := g.ssh("true")
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("no SSH over vsock/%d within 3m: %v\n%s\nconsole:\n%s", g.cid, err, out, consoleTail(g.l))
		}
		time.Sleep(5 * time.Second)
	}
}

func (g *guest) run(cmd string) {
	g.t.Helper()
	if out, err := g.ssh(cmd); err != nil {
		g.t.Fatalf("guest command %q: %v\n%s", cmd, err, out)
	}
}

func (g *guest) poweroff() {
	g.t.Helper()
	_, _ = g.ssh("systemctl poweroff")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		state, _, err := g.l.DomainGetState(g.dom, 0)
		must(g.t, err, "domain state")
		if golibvirt.DomainState(state) == golibvirt.DomainShutoff {
			return
		}
		if time.Now().After(deadline) {
			g.t.Fatal("domain did not shut off within 2m")
		}
		time.Sleep(2 * time.Second)
	}
}

func (g *guest) ssh(cmd string) ([]byte, error) {
	host := fmt.Sprintf("vsock/%d", g.cid)
	c := exec.Command("ssh", "-F", "/dev/null",
		"-i", g.key,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ProxyCommand="+sshProxy+" %h %p",
		"-o", "ProxyUseFdpass=yes",
		"root@"+host, cmd)
	return c.CombinedOutput()
}

func ensurePool(t *testing.T, l *golibvirt.Libvirt) golibvirt.StoragePool {
	t.Helper()
	pool, err := l.StoragePoolLookupByName(testPool)
	if err != nil {
		xml := fmt.Sprintf("<pool type='dir'><name>%s</name><target><path>%s</path></target></pool>", testPool, testPoolPath)
		pool, err = l.StoragePoolDefineXML(xml, 0)
		must(t, err, "define pool")
		must(t, l.StoragePoolBuild(pool, 0), "build pool")
	}
	active, err := l.StoragePoolIsActive(pool)
	must(t, err, "pool state")
	if active == 0 {
		must(t, l.StoragePoolCreate(pool, 0), "start pool")
	}
	must(t, l.StoragePoolRefresh(pool, 0), "refresh pool")
	return pool
}

// ensureBaseVolume uploads the cloud image once; later runs reuse it.
func ensureBaseVolume(t *testing.T, l *golibvirt.Libvirt, pool golibvirt.StoragePool, image string) string {
	t.Helper()
	if vol, err := l.StorageVolLookupByName(pool, baseVolume); err == nil {
		path, err := l.StorageVolGetPath(vol)
		must(t, err, "base volume path")
		return path
	}
	vol := uploadVolume(t, l, pool, baseVolume, image)
	must(t, l.StoragePoolRefresh(pool, 0), "refresh pool")
	path, err := l.StorageVolGetPath(vol)
	must(t, err, "base volume path")
	return path
}

func createOverlay(t *testing.T, l *golibvirt.Libvirt, pool golibvirt.StoragePool, base string) string {
	t.Helper()
	xml := fmt.Sprintf(`<volume><name>%s</name><capacity unit='G'>10</capacity>
<target><format type='qcow2'/></target>
<backingStore><path>%s</path><format type='qcow2'/></backingStore></volume>`, diskVolume, base)
	vol, err := l.StorageVolCreateXML(pool, xml, 0)
	must(t, err, "create overlay")
	path, err := l.StorageVolGetPath(vol)
	must(t, err, "overlay path")
	return path
}

// createQcow2Vars converts the raw OVMF vars template into a qcow2 volume,
// the format that lets libvirt snapshot the NVRAM.
func createQcow2Vars(t *testing.T, l *golibvirt.Libvirt, pool golibvirt.StoragePool) string {
	t.Helper()
	raw := uploadVolume(t, l, pool, varsRawVolume, ovmfVars)
	xml := fmt.Sprintf("<volume><name>%s</name><capacity>0</capacity><target><format type='qcow2'/></target></volume>", varsVolume)
	vol, err := l.StorageVolCreateXMLFrom(pool, xml, raw, 0)
	must(t, err, "convert vars to qcow2")
	must(t, l.StorageVolDelete(raw, 0), "delete raw vars")
	path, err := l.StorageVolGetPath(vol)
	must(t, err, "vars path")
	return path
}

func uploadVolume(t *testing.T, l *golibvirt.Libvirt, pool golibvirt.StoragePool, name, file string) golibvirt.StorageVol {
	t.Helper()
	f, err := os.Open(file)
	must(t, err, "open "+file)
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	must(t, err, "stat "+file)
	xml := fmt.Sprintf("<volume><name>%s</name><capacity unit='bytes'>%d</capacity><target><format type='raw'/></target></volume>", name, st.Size())
	vol, err := l.StorageVolCreateXML(pool, xml, 0)
	must(t, err, "create volume "+name)
	must(t, l.StorageVolUpload(vol, f, 0, uint64(st.Size()), 0), "upload "+name)
	return vol
}

// hostKeyDropin makes sshd create its host keys. A cloud image ships without
// them and leaves that to cloud-init, which has no datasource here.
var hostKeyDropin = base64.StdEncoding.EncodeToString([]byte("[Service]\nExecStartPre=/usr/bin/ssh-keygen -A\n"))

func domainXML(disk, vars, publicKey string) string {
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
    </oemStrings>
  </sysinfo>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <interface type='network'><source network='default'/><model type='virtio'/></interface>
    <vsock model='virtio'><cid auto='yes'/></vsock>
    <serial type='file'><source path='%s'/></serial>
  </devices>
</domain>`, testDomain, ovmfCode, vars, strings.TrimSpace(publicKey), hostKeyDropin, disk, testPoolPath+"/"+consoleLog)
}

func liveCID(t *testing.T, l *golibvirt.Libvirt, dom golibvirt.Domain) uint {
	t.Helper()
	desc, err := l.DomainGetXMLDesc(dom, 0)
	must(t, err, "live domain XML")
	var d libvirtxml.Domain
	must(t, d.Unmarshal(desc), "parse domain XML")
	if d.Devices == nil || d.Devices.VSock == nil || d.Devices.VSock.CID == nil || d.Devices.VSock.CID.Address == "" {
		t.Fatal("live domain XML has no vsock CID")
	}
	cid, err := strconv.ParseUint(d.Devices.VSock.CID.Address, 10, 32)
	must(t, err, "parse vsock CID")
	return uint(cid)
}

// assertSnapshotCoversNVRAM guards against libvirt's raw-NVRAM trap: a snapshot
// that silently leaves the firmware state out.
func assertSnapshotCoversNVRAM(t *testing.T, l *golibvirt.Libvirt, snap golibvirt.DomainSnapshot) {
	t.Helper()
	desc, err := l.DomainSnapshotGetXMLDesc(snap, 0)
	must(t, err, "snapshot XML")
	var s libvirtxml.DomainSnapshot
	must(t, s.Unmarshal(desc), "parse snapshot XML")
	if s.Domain == nil || s.Domain.OS == nil || s.Domain.OS.NVRam == nil || s.Domain.OS.NVRam.Format != "qcow2" {
		t.Fatalf("snapshot does not record a qcow2 NVRAM:\n%s", desc)
	}
}

type sshKey struct{ private, public string }

func newSSHKey(t *testing.T) sshKey {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "werft-test", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(path + ".pub")
	must(t, err, "read public key")
	return sshKey{private: path, public: string(bytes.TrimSpace(pub))}
}

// consoleTail reads the serial log through the pool, which needs no root.
func consoleTail(l *golibvirt.Libvirt) string {
	pool, err := l.StoragePoolLookupByName(testPool)
	if err != nil {
		return err.Error()
	}
	_ = l.StoragePoolRefresh(pool, 0)
	vol, err := l.StorageVolLookupByName(pool, consoleLog)
	if err != nil {
		return err.Error()
	}
	var buf bytes.Buffer
	if err := l.StorageVolDownload(vol, &buf, 0, 0, 0); err != nil {
		return err.Error()
	}
	lines := strings.Split(buf.String(), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	return strings.Join(lines, "\n")
}

func removeDomain(l *golibvirt.Libvirt) {
	dom, err := l.DomainLookupByName(testDomain)
	if err != nil {
		return
	}
	_ = l.DomainDestroy(dom)
	_ = l.DomainUndefineFlags(dom, golibvirt.DomainUndefineSnapshotsMetadata|golibvirt.DomainUndefineKeepNvram)
}

func deleteVolumes(l *golibvirt.Libvirt, pool golibvirt.StoragePool, names ...string) {
	for _, name := range names {
		if vol, err := l.StorageVolLookupByName(pool, name); err == nil {
			_ = l.StorageVolDelete(vol, 0)
		}
	}
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		var lerr golibvirt.Error
		if errors.As(err, &lerr) {
			t.Fatalf("%s: %s", what, lerr.Message)
		}
		t.Fatalf("%s: %v", what, err)
	}
}
