//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
)

// identity is the machine-id, hostname file and ed25519 host key of a guest.
const identity = "cat /etc/machine-id; cat /etc/hostname 2>/dev/null || echo no-hostname; cat /etc/ssh/ssh_host_ed25519_key.pub"

// TestSealLive gives a scratch domain the user and keys prepare leaves, takes
// its provision-base, seals it with werft seal, and boots a lease-like
// overlay of the stock disk. The overlay must come up with a new machine-id
// and new SSH host keys, made by the drop-in seal installs: its domain has
// no host-key credential of its own.
func TestSealLive(t *testing.T) {
	isolate(t)
	h := livetest.Connect(t)
	src := livetest.NewGuest(t, h, "werft-test-sl-src")
	u, err := user.Current()
	livetest.Must(t, err, "current user")
	dir, err := state.Root()
	livetest.Must(t, err, "state directory")
	livetest.Must(t, guest.EnsureKey(dir.SSHKey()), "create the werft key")
	werftKey := publicKey(dir.SSHKey() + ".pub")
	ownKey := livetest.NewKey(t)

	src.Boot()
	addImageUser(src, u.Username, werftKey, ownKey.Public)
	before := src.Run(identity)
	src.Poweroff()
	werftJSON(t, 0, "snapshot", src.Name, "provision-base")
	src.Boot()
	src.Run("touch /root/after-provision-base && sync")
	src.Poweroff()

	werftJSON(t, 2, "seal", "--yes", src.Name, "clean-install")
	werftJSON(t, 5, "seal", src.Name, "provision-base")
	doc := werftJSON(t, 0, "seal", "--yes", src.Name, "provision-base")
	name, _ := doc["name"].(string)
	disk, nvram := name+".qcow2", name+"_VARS.qcow2"
	t.Cleanup(func() {
		livetest.RemoveDomain(t, h, name)
		livetest.DeleteVolumes(h, disk, nvram)
	})
	version := time.Now().Format("20060102")
	if name != src.Name+"-plain-"+version || doc["image"] != src.Name || doc["tier"] != "plain" || doc["version"] != version ||
		doc["user"] != u.Username || doc["family"] != "ubuntu" || doc["disk"] != disk || doc["nvram"] != nvram || doc["pool"] != livetest.Pool {
		t.Fatalf("seal = %v", doc)
	}
	if v, _ := doc["systemd"].(float64); v < 256 {
		t.Errorf("seal recorded systemd %v, want 256 or later", doc["systemd"])
	}
	if _, err := h.Domain(name); !failure.Is(err, failure.NotFound) {
		t.Errorf("the temporary domain %s is left: %v", name, err)
	}
	images := werftList(t, "images")
	if len(images) != 1 || images[0]["name"] != name || images[0]["image"] != src.Name || images[0]["tier"] != "plain" ||
		images[0]["version"] != version || images[0]["date"] == nil {
		t.Errorf("images = %v", images)
	}

	// Control: the build VM keeps its identity across a boot, and seal reset
	// it to provision-base.
	src.Boot()
	if got := src.Run(identity); got != before {
		t.Errorf("identity of %s after seal = %q, want %q", src.Name, got, before)
	}
	src.Run("test ! -e /root/after-provision-base")
	src.Poweroff()

	inspectStockDisk(t, h, disk)

	var seen [][]string
	for _, lease := range []string{"werft-test-sl-lease1", "werft-test-sl-lease2"} {
		g := leaseOf(t, h, lease, disk, nvram)
		g.Boot()
		got := strings.Split(g.Run(identity), "\n")
		if got[1] != "no-hostname" {
			t.Errorf("%s has /etc/hostname %q", lease, got[1])
		}
		keys := g.Run(fmt.Sprintf("cat ~%s/.ssh/authorized_keys", u.Username))
		if strings.Contains(keys, werftKey) || !strings.Contains(keys, ownKey.Public) {
			t.Errorf("authorized_keys of %s = %q, want the own key without the werft key", lease, keys)
		}
		g.Run(fmt.Sprintf("runuser -u %s -- sudo -n true", u.Username))
		g.Run("test ! -e /root/after-provision-base")
		g.Run("systemctl start ssh.service && systemctl is-active ssh.service")
		g.Poweroff()
		seen = append(seen, got)
	}
	// Every boot of the image must make its own machine-id and host keys:
	// keys baked into the stock disk would be shared by every lease.
	was := strings.Split(before, "\n")
	for _, i := range []int{0, 2} {
		a, b := seen[0][i], seen[1][i]
		if a == "" || b == "" || a == b || a == was[i] || b == was[i] {
			t.Errorf("line %d of the identity: build VM %q, first lease %q, second lease %q; want three different ones", i, was[i], a, b)
		}
	}
}

// addImageUser gives the running src what prepare leaves in a build VM: the
// user with the werft key and the own key, and a static hostname.
func addImageUser(src *livetest.Guest, user, werftKey, ownKey string) {
	src.T.Helper()
	src.Run(fmt.Sprintf(`set -e
useradd -m -s /bin/sh %[1]s
install -d -m 700 -o %[1]s -g %[1]s ~%[1]s/.ssh
printf '%%s werft\n%%s\n' '%[2]s' '%[3]s' > ~%[1]s/.ssh/authorized_keys
chown %[1]s: ~%[1]s/.ssh/authorized_keys
echo %[4]s > /etc/hostname
sync`, user, werftKey, ownKey, src.Name))
}

// inspectStockDisk reads the root file system of the stock disk. The disk
// must hold no SSH host keys, no hostname and an empty machine-id. It returns
// the debugfs reader of that file system.
func inspectStockDisk(t *testing.T, h virt.Hypervisor, disk string) func(cmd string) string {
	t.Helper()
	debugfs := stockDebugfs(t, h, disk)
	if ssh := debugfs("ls /etc/ssh"); !strings.Contains(ssh, "sshd_config") || strings.Contains(ssh, "ssh_host_") {
		t.Errorf("/etc/ssh of the stock disk, want sshd_config and no host keys:\n%s", ssh)
	}
	if etc := debugfs("ls /etc"); !strings.Contains(etc, "machine-id") || strings.Contains(etc, " hostname ") {
		t.Errorf("/etc of the stock disk, want machine-id and no hostname:\n%s", etc)
	}
	if id := debugfs("stat /etc/machine-id"); !strings.Contains(id, "Size: 0") {
		t.Errorf("/etc/machine-id of the stock disk is not empty:\n%s", id)
	}
	return debugfs
}

// stockDebugfs downloads the stock disk without root, converts it to raw, and
// returns a function that runs a debugfs command on its largest partition.
func stockDebugfs(t *testing.T, h virt.Hypervisor, disk string) func(cmd string) string {
	t.Helper()
	dir := t.TempDir()
	qcow2, raw := filepath.Join(dir, "disk.qcow2"), filepath.Join(dir, "disk.raw")
	f, err := os.Create(qcow2)
	livetest.Must(t, err, "create "+qcow2)
	livetest.Must(t, h.DownloadVolume(livetest.Pool, disk, f), "download "+disk)
	livetest.Must(t, f.Close(), "close "+qcow2)
	if out, err := exec.Command("qemu-img", "convert", "-O", "raw", qcow2, raw).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert: %v\n%s", err, out)
	}
	livetest.Must(t, os.Remove(qcow2), "remove "+qcow2)
	out, err := exec.Command("/usr/sbin/sfdisk", "-J", raw).Output()
	livetest.Must(t, err, "sfdisk")
	var table struct {
		PartitionTable struct {
			SectorSize uint64 `json:"sectorsize"`
			Partitions []struct {
				Start uint64 `json:"start"`
				Size  uint64 `json:"size"`
			} `json:"partitions"`
		} `json:"partitiontable"`
	}
	livetest.Must(t, json.Unmarshal(out, &table), "parse the partition table")
	var start, size uint64
	for _, p := range table.PartitionTable.Partitions {
		if p.Size > size {
			start, size = p.Start, p.Size
		}
	}
	fs := fmt.Sprintf("%s?offset=%d", raw, start*table.PartitionTable.SectorSize)
	return func(cmd string) string {
		t.Helper()
		out, err := exec.Command("/usr/sbin/debugfs", "-R", cmd, fs).CombinedOutput()
		livetest.Must(t, err, "debugfs "+cmd)
		return string(out)
	}
}

// leaseOf is a scratch guest on a qcow2 overlay of the stock disk and a copy
// of the stock NVRAM, as a lease boots, without the host-key credential of
// livetest.
func leaseOf(t *testing.T, h virt.Hypervisor, name, disk, nvram string) *livetest.Guest {
	t.Helper()
	overlay, vars := name+"-overlay.qcow2", name+"-nvram.qcow2"
	livetest.DeleteVolumes(h, overlay, vars)
	t.Cleanup(func() { livetest.DeleteVolumes(h, overlay, vars) })
	base, err := h.VolumePath(livetest.Pool, disk)
	livetest.Must(t, err, "stock disk path")
	livetest.Overlay(t, h, overlay, base)
	_, err = h.CreateVolumeFrom(livetest.Pool, fmt.Sprintf("<volume><name>%s</name><capacity>0</capacity><target><format type='qcow2'/></target></volume>", vars), livetest.Pool, nvram)
	livetest.Must(t, err, "copy the stock NVRAM")
	g := guestOn(t, h, name, overlay, vars)
	text, err := h.DomainXML(name, true)
	livetest.Must(t, err, "domain XML")
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "systemd.unit-dropin.sshd-vsock@.service") {
			kept = append(kept, line)
		}
	}
	if len(kept) == len(strings.Split(text, "\n")) {
		t.Fatalf("the domain XML of %s has no host-key credential to remove", name)
	}
	_, err = h.DefineDomain(strings.Join(kept, "\n"))
	livetest.Must(t, err, "redefine "+name)
	return g
}

// werftList runs werft --json with args and parses the JSON array it prints.
func werftList(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{stdout: &out, stderr: &errOut, dial: dialLibvirt}
	if code := c.run(append([]string{"--json"}, args...)); code != 0 {
		t.Fatalf("werft %q exited %d: %s", args, code, errOut.String())
	}
	var list []map[string]any
	livetest.Must(t, json.Unmarshal(out.Bytes(), &list), "parse the output of werft "+strings.Join(args, " "))
	return list
}

// TestSealFullLive seals the clean reset point of a scratch -full domain and
// leases the full tier. The lease must reach SSH with a new machine-id and
// hold what the -full domain had at clean. Control: a file written after
// clean is not in the lease, provision-base of the -full domain is refused,
// and the image has no plain tier.
func TestSealFullLive(t *testing.T) {
	isolate(t)
	h := livetest.Connect(t)
	prefix := livetest.DomainPrefix + "sf-"
	old := leasePrefix
	leasePrefix = prefix
	t.Cleanup(func() { leasePrefix = old })

	img := prefix + "vm"
	src := livetest.NewGuest(t, h, img+"-full")
	u, err := user.Current()
	livetest.Must(t, err, "current user")
	dir, err := state.Root()
	livetest.Must(t, err, "state directory")
	livetest.Must(t, guest.EnsureKey(dir.SSHKey()), "create the werft key")
	src.Boot()
	addImageUser(src, u.Username, publicKey(dir.SSHKey()+".pub"), livetest.NewKey(t).Public)
	src.Run("touch /root/at-clean && sync")
	machineID := src.Run("cat /etc/machine-id")
	src.Poweroff()
	werftJSON(t, 0, "snapshot", src.Name, "clean")
	werftJSON(t, 0, "snapshot", src.Name, "provision-base")
	src.Boot()
	src.Run("touch /root/after-clean && sync")
	src.Poweroff()

	werftJSON(t, 2, "seal", "--yes", src.Name, "provision-base")
	doc := werftJSON(t, 0, "seal", "--yes", src.Name, "clean")
	stock, _ := doc["name"].(string)
	t.Cleanup(func() {
		livetest.RemoveDomain(t, h, stock)
		livetest.DeleteVolumes(h, stock+".qcow2", stock+"_VARS.qcow2")
	})
	version := time.Now().Format("20060102")
	if stock != img+"-full-"+version || doc["image"] != img || doc["tier"] != "full" || doc["source"] != src.Name || doc["label"] != "clean" {
		t.Fatalf("seal = %v", doc)
	}
	images := werftList(t, "images")
	if len(images) != 1 || images[0]["name"] != stock || images[0]["image"] != img || images[0]["tier"] != "full" {
		t.Errorf("images = %v", images)
	}

	werftJSON(t, 3, "lease", img)
	lease := parallelLeases(t, h, prefix, []string{"lease", img, "--tier", "full"})[0]
	id, _ := lease["id"].(string)
	if lease["domain"] != prefix+img+"-full-"+id || lease["stock"] != stock || lease["user"] != u.Username {
		t.Errorf("lease = %v", lease)
	}
	out := leaseSSH(t, lease["ssh"].(string), "cat /etc/machine-id; sudo -n test -e /root/at-clean && echo at-clean; sudo -n test -e /root/after-clean || echo no-after-clean")
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || len(lines[0]) != 32 || lines[0] == machineID || lines[1] != "at-clean" || lines[2] != "no-after-clean" {
		t.Errorf("machine-id and files of the full lease:\n%s\nwant a machine-id other than %s, at-clean and no-after-clean", out, machineID)
	}
}

// TestSeal255Live seals a scratch guest with systemd 255 and takes two leases
// of it. The seal logs in as root at the DHCP address with a run key, so the
// stock disk must hold no key but the one the guest had. Each lease must have
// its own identity, no vsock device, and a login that survives a reboot.
// Control: the root key of the source is still on the stock disk, so the read
// that finds no run key reads the real file.
func TestSeal255Live(t *testing.T) {
	isolate(t)
	h := livetest.Connect(t)
	prefix := livetest.DomainPrefix + "s255-"
	old := leasePrefix
	leasePrefix = prefix
	t.Cleanup(func() { leasePrefix = old })

	src := livetest.NewTCPGuest(t, h, prefix+"src")
	u, err := user.Current()
	livetest.Must(t, err, "current user")
	dir, err := state.Root()
	livetest.Must(t, err, "state directory")
	livetest.Must(t, guest.EnsureKey(dir.SSHKey()), "create the werft key")
	src.Boot()
	if v := strings.TrimSpace(src.Run(`systemctl --version | sed -n '1s/^systemd \([0-9]*\).*/\1/p'`)); v != "255" {
		t.Fatalf("the test image has systemd %q, want 255", v)
	}
	addImageUser(src, u.Username, publicKey(dir.SSHKey()+".pub"), livetest.NewKey(t).Public)
	src.Poweroff()
	livetest.Must(t, h.SetDomainDescription(src.Name, domxml.SystemdLine(255)), "set the systemd line")
	werftJSON(t, 0, "snapshot", src.Name, "provision-base")

	doc := werftJSON(t, 0, "seal", "--yes", src.Name, "provision-base")
	stock, _ := doc["name"].(string)
	t.Cleanup(func() {
		livetest.RemoveDomain(t, h, stock)
		livetest.DeleteVolumes(h, stock+".qcow2", stock+"_VARS.qcow2")
	})
	if v, _ := doc["systemd"].(float64); v != 255 {
		t.Errorf("seal recorded systemd %v, want 255", doc["systemd"])
	}

	debugfs := inspectStockDisk(t, h, stock+".qcow2")
	var rootKeys []string
	for _, line := range strings.Split(debugfs("cat /root/.ssh/authorized_keys"), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "ssh-") {
			rootKeys = append(rootKeys, line)
		}
	}
	// tmpfiles appends the key of the source at each of its boots.
	if len(rootKeys) == 0 || slices.ContainsFunc(rootKeys, func(k string) bool { return k != src.Key.Public }) {
		t.Errorf("root keys of the stock disk = %q, want only the key of the source", rootKeys)
	}
	if dbus := debugfs("stat /var/lib/dbus/machine-id"); strings.Contains(dbus, "Type: regular") {
		t.Errorf("/var/lib/dbus/machine-id of the stock disk is a regular file:\n%s", dbus)
	}

	docs := parallelLeases(t, h, prefix, []string{"lease", src.Name}, []string{"lease", src.Name})
	var ids, hostKeys, ips []string
	for _, l := range docs {
		name, _ := l["domain"].(string)
		ssh, _ := l["ssh"].(string)
		address, _ := l["address"].(string)
		if net.ParseIP(address) == nil {
			t.Errorf("address of %s = %q, want an IP", name, address)
		}
		if got := leaseSSH(t, ssh, "hostname"); got != name {
			t.Errorf("hostname of the lease = %q, want %s", got, name)
		}
		if got := leaseSSH(t, ssh, "sudo -n true 2>&1"); got != "" {
			t.Errorf("sudo -n true in %s printed %q", name, got)
		}
		text, err := h.DomainXML(name, false)
		livetest.Must(t, err, "live XML of "+name)
		d, err := domxml.Parse(text)
		livetest.Must(t, err, "parse the live XML of "+name)
		if d.Devices != nil && d.Devices.VSock != nil {
			t.Errorf("%s has a vsock device", name)
		}
		if got := listEntry(t, name)["address"]; got != address {
			t.Errorf("werft list shows the address %v of %s, want %s", got, name, address)
		}
		ids = append(ids, leaseSSH(t, ssh, "cat /etc/machine-id"))
		hostKeys = append(hostKeys, leaseSSH(t, ssh, "cat /etc/ssh/ssh_host_ed25519_key.pub"))
		ips = append(ips, address)

		rebootLease(t, name, ssh)
		if got := leaseSSH(t, ssh, "hostname"); got != name {
			t.Errorf("hostname of the lease after a reboot = %q, want %s", got, name)
		}
	}
	for what, pair := range map[string][]string{"machine-id": ids, "host key": hostKeys, "IP": ips} {
		if pair[0] == "" || pair[0] == pair[1] {
			t.Errorf("the two leases have the %s %q and %q, want two different ones", what, pair[0], pair[1])
		}
	}
}

// rebootLease reboots the lease from inside and waits for a login to the new
// boot through the same ssh command.
func rebootLease(t *testing.T, name, ssh string) {
	t.Helper()
	const bootID = "cat /proc/sys/kernel/random/boot_id"
	before := leaseSSH(t, ssh, bootID)
	_, _ = leaseAttempt(ssh, "sudo -n systemctl reboot")
	deadline := time.Now().Add(3 * time.Minute)
	for {
		now, err := leaseAttempt(ssh, bootID)
		if err == nil && now != "" && now != before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ssh command of %s gives no login to a new boot within 3m: %v", name, err)
		}
		time.Sleep(3 * time.Second)
	}
}

// leaseAttempt runs cmd with a time limit: a connection that the reboot cut
// gets no reset from the guest, and ssh would wait on it for minutes.
func leaseAttempt(ssh, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", append(strings.Fields(ssh)[1:], "--", cmd)...).Output()
	return strings.TrimSpace(string(out)), err
}
