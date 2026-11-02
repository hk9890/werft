//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/user"
	"strings"
	"testing"
	"time"

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
	src.Run(fmt.Sprintf(`set -e
useradd -m -s /bin/sh %[1]s
install -d -m 700 -o %[1]s -g %[1]s ~%[1]s/.ssh
printf '%%s werft\n%%s\n' '%[2]s' '%[3]s' > ~%[1]s/.ssh/authorized_keys
chown %[1]s: ~%[1]s/.ssh/authorized_keys
echo %[4]s > /etc/hostname
sync`, u.Username, werftKey, ownKey.Public, src.Name))
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

	lease := leaseOf(t, h, "werft-test-sl-lease", disk, nvram)
	lease.Boot()
	after := strings.Split(lease.Run(identity), "\n")
	was := strings.Split(before, "\n")
	if after[0] == "" || after[0] == was[0] {
		t.Errorf("machine-id of the sealed copy = %q, the build VM had %q", after[0], was[0])
	}
	if after[1] != "no-hostname" {
		t.Errorf("the sealed copy has /etc/hostname %q", after[1])
	}
	if after[2] == "" || after[2] == was[2] {
		t.Errorf("host key of the sealed copy = %q, the build VM had %q", after[2], was[2])
	}
	keys := lease.Run(fmt.Sprintf("cat ~%s/.ssh/authorized_keys", u.Username))
	if strings.Contains(keys, werftKey) || !strings.Contains(keys, ownKey.Public) {
		t.Errorf("authorized_keys of the sealed copy = %q, want the own key without the werft key", keys)
	}
	lease.Run(fmt.Sprintf("runuser -u %s -- sudo -n true", u.Username))
	lease.Run("test ! -e /root/after-provision-base")
	lease.Run("systemctl start ssh.service && systemctl is-active ssh.service")
	lease.Poweroff()
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
