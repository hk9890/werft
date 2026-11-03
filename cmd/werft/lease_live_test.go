//go:build live

package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
)

// TestLeaseLive seals a scratch domain, leases a VM from the stock image,
// logs in with the ssh command werft lease prints, and releases the lease.
// Control: a key other than the lease key gets no login, and the lease key
// does with the same command line.
func TestLeaseLive(t *testing.T) {
	isolate(t)
	h := livetest.Connect(t)
	prefix := livetest.DomainPrefix + "ls-"
	old := leasePrefix
	leasePrefix = prefix
	t.Cleanup(func() { leasePrefix = old })

	src := livetest.NewGuest(t, h, prefix+"src")
	u, err := user.Current()
	livetest.Must(t, err, "current user")
	dir, err := state.Root()
	livetest.Must(t, err, "state directory")
	livetest.Must(t, guest.EnsureKey(dir.SSHKey()), "create the werft key")
	ownKey := livetest.NewKey(t)
	src.Boot()
	addImageUser(src, u.Username, publicKey(dir.SSHKey()+".pub"), ownKey.Public)
	src.Poweroff()
	werftJSON(t, 0, "snapshot", src.Name, "provision-base")
	stock, _ := werftJSON(t, 0, "seal", "--yes", src.Name, "provision-base")["name"].(string)
	t.Cleanup(func() {
		livetest.RemoveDomain(t, h, stock)
		livetest.DeleteVolumes(h, stock+".qcow2", stock+"_VARS.qcow2")
	})

	werftJSON(t, 3, "lease", "no-such-image")
	doc := werftJSON(t, 0, "lease", src.Name, "--tier", "plain", "--owner", "agent-ls")
	id, _ := doc["id"].(string)
	name, _ := doc["domain"].(string)
	t.Cleanup(func() { removeLease(t, h, prefix, id, name) })
	sshConfig, _ := doc["ssh_config"].(string)
	key, _ := doc["key"].(string)
	address, _ := doc["address"].(string)
	if name != prefix+src.Name+"-plain-"+id || doc["user"] != u.Username || doc["owner"] != "agent-ls" || doc["stock"] != stock ||
		!strings.HasPrefix(address, "vsock/") || doc["ssh"] != "ssh -F "+sshConfig+" "+id ||
		key != filepath.Join(dir.Lease(id), "id_ed25519") || doc["expiry"] == nil {
		t.Fatalf("lease = %v", doc)
	}
	meta, err := h.DomainMetadata(name, lease.Namespace)
	if err != nil || !strings.Contains(meta, "agent-ls") || !strings.Contains(meta, stock) {
		t.Errorf("lease metadata = %q, %v", meta, err)
	}
	text, err := h.DomainXML(name, true)
	livetest.Must(t, err, "lease domain XML")
	d, err := domxml.Parse(text)
	livetest.Must(t, err, "parse the lease domain XML")
	nvram, _ := domxml.NVRAMOf(d)

	out := leaseSSH(t, doc["ssh"].(string), "hostname; cat /etc/machine-id; sudo -n cat ~/.ssh/authorized_keys")
	lines := strings.SplitN(out, "\n", 3)
	pub, err := os.ReadFile(key + ".pub")
	livetest.Must(t, err, "read the lease key")
	if len(lines) != 3 || lines[0] != name || len(lines[1]) != 32 ||
		!strings.Contains(lines[2], strings.TrimSpace(string(pub))) || !strings.Contains(lines[2], ownKey.Public) {
		t.Errorf("hostname, machine-id and authorized_keys of the lease:\n%s\nwant %s, a machine-id, the lease key and the own key", out, name)
	}

	// Control: the same login with a key that is not the lease key fails.
	wrong := livetest.NewKey(t)
	login := func(key string) error {
		return exec.Command("ssh", "-F", "/dev/null", "-i", key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"-o", "ProxyCommand="+guest.SSHProxy+" %h %p", "-o", "ProxyUseFdpass=yes",
			u.Username+"@"+address, "true").Run()
	}
	if err := login(wrong.Private); err == nil {
		t.Errorf("a key other than the lease key logs in to %s", name)
	}
	if err := login(key); err != nil {
		t.Errorf("the lease key does not log in to %s with the control command: %v", name, err)
	}

	if doc := werftJSON(t, 0, "release", id); doc["domain"] != name {
		t.Errorf("release = %v", doc)
	}
	if _, err := h.Domain(name); !failure.Is(err, failure.NotFound) {
		t.Errorf("%s is left after release: %v", name, err)
	}
	volumes, err := h.Volumes(livetest.Pool)
	livetest.Must(t, err, "list the volumes")
	if slices.Contains(volumes, name+".qcow2") {
		t.Errorf("the overlay %s.qcow2 is left after release", name)
	}
	if _, err := os.Stat(dir.Lease(id)); !os.IsNotExist(err) {
		t.Errorf("the lease directory is left after release: %v", err)
	}
	if _, err := os.Stat(nvram.Path); nvram.Path == "" || !os.IsNotExist(err) {
		t.Errorf("the lease NVRAM %q is left after release: %v", nvram.Path, err)
	}
	if doc := werftJSON(t, 0, "release", id); doc["domain"] != "" {
		t.Errorf("second release = %v", doc)
	}
}

// leaseSSH runs the ssh command werft lease printed, with cmd.
func leaseSSH(t *testing.T, ssh, cmd string) string {
	t.Helper()
	args := append(strings.Fields(ssh)[1:], "--", cmd)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s -- %q: %v\n%s", ssh, cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

// removeLease releases a lease the test left. It refuses a lease domain
// without the prefix of a test domain.
func removeLease(t *testing.T, h virt.Hypervisor, prefix, id, name string) {
	t.Helper()
	if id == "" {
		return
	}
	if !strings.HasPrefix(name, prefix) {
		t.Fatalf("lease domain %q does not start with %q", name, prefix)
	}
	dir, err := state.Root()
	livetest.Must(t, err, "state directory")
	if _, err := lease.Release(h, dir, prefix, id); err != nil {
		t.Errorf("release %s in cleanup: %v", id, err)
	}
}
