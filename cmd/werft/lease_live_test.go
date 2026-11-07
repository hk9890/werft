//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
)

// TestLeaseLive seals a scratch domain, refuses a lease over a budget of
// 1 PiB, starts two leases of the stock image at the same time, logs in to
// both with the ssh command werft lease prints, and releases the first. Then it checks the lifetime of the second
// (leaseLifetime). Control: the stock definition writes the serial console
// to one file, which the two leases would share; a key other than the lease
// key gets no login, and the lease key does with the same command line.
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

	rec, err := lease.Resolve(dir.Images(), src.Name, "plain", "")
	livetest.Must(t, err, "resolve the stock image")
	if !strings.Contains(rec.Domain, src.ConsoleVolume()) {
		t.Fatalf("the stock definition writes the console to no file:\n%s", rec.Domain)
	}

	werftJSON(t, 3, "lease", "no-such-image")
	config := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "werft", "config.toml")
	cfg, err := os.ReadFile(config)
	livetest.Must(t, err, "read config.toml")
	livetest.Must(t, os.WriteFile(config, append(slices.Clone(cfg), "memory_budget_mib = 1073741824\n"...), 0o600), "write a budget of 1 PiB")
	if doc := werftJSON(t, 6, "lease", src.Name); !strings.Contains(fmt.Sprint(doc["error"]), "no capacity") {
		t.Errorf("lease over a budget of 1 PiB = %v, want no capacity", doc)
	}
	livetest.Must(t, os.WriteFile(config, cfg, 0o600), "restore config.toml")
	docs := parallelLeases(t, h, prefix, []string{"lease", src.Name, "--tier", "plain", "--owner", "agent-ls"}, []string{"lease", src.Name})
	doc, second := docs[0], docs[1]
	id, _ := doc["id"].(string)
	name, _ := doc["domain"].(string)
	if second["id"] == id || second["domain"] == name || second["address"] == doc["address"] {
		t.Errorf("the parallel leases share an id, a domain or an address:\n%v\n%v", doc, second)
	}
	if out := leaseSSH(t, second["ssh"].(string), "hostname"); out != second["domain"] {
		t.Errorf("hostname of the second lease = %q, want %v", out, second["domain"])
	}
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

	leaseLifetime(t, h, second)
}

// parallelLeases runs werft lease with each of args at the same time, and
// returns the JSON documents in the order of args. Every lease is released
// when the test ends.
func parallelLeases(t *testing.T, h virt.Hypervisor, prefix string, args ...[]string) []map[string]any {
	t.Helper()
	type result struct {
		code        int
		out, errOut bytes.Buffer
	}
	results := make([]result, len(args))
	var wg sync.WaitGroup
	for i, a := range args {
		wg.Go(func() {
			r := &results[i]
			c := &cli{stdout: &r.out, stderr: &r.errOut, dial: dialLibvirt, sleep: time.Sleep, openViewer: func(string) error { return nil }}
			r.code = c.run(append([]string{"--json"}, a...))
		})
	}
	wg.Wait()
	docs := make([]map[string]any, len(args))
	for i := range results {
		r := &results[i]
		_ = json.Unmarshal(r.out.Bytes(), &docs[i]) // a failed run is reported below, with its output
		id, _ := docs[i]["id"].(string)
		name, _ := docs[i]["domain"].(string)
		t.Cleanup(func() { removeLease(t, h, prefix, id, name) })
	}
	for i := range results {
		if r := &results[i]; r.code != 0 {
			t.Fatalf("werft %q exited %d\nstdout: %s\nstderr: %s", args[i], r.code, r.out.String(), r.errOut.String())
		}
	}
	return docs
}

// leaseLifetime extends the lease of doc by an hour, and moves the clock.
// Control: one minute after the first expiry the extended lease stays, and
// werft leases removes only an orphan lease directory. One minute after the
// new expiry werft leases releases it.
func leaseLifetime(t *testing.T, h virt.Hypervisor, doc map[string]any) {
	dir, err := state.Root()
	livetest.Must(t, err, "state directory")
	id, _ := doc["id"].(string)
	name, _ := doc["domain"].(string)
	expiry, err := time.Parse(time.RFC3339, doc["expiry"].(string))
	livetest.Must(t, err, "parse the expiry of "+id)
	if ids := leaseIDs(t); !slices.Equal(ids, []string{id}) {
		t.Errorf("werft leases = %v, want %s", ids, id)
	}

	extended := expiry.Add(time.Hour)
	if doc := werftJSON(t, 0, "extend", id, "--for", "1h"); doc["expiry"] != extended.Format(time.RFC3339) {
		t.Errorf("extend --for 1h = %v, want the expiry %v", doc["expiry"], extended)
	}
	meta, err := h.DomainMetadata(name, lease.Namespace)
	if err != nil || !strings.Contains(meta, extended.Format(time.RFC3339)) {
		t.Errorf("lease metadata after extend = %q, %v", meta, err)
	}

	livetest.Must(t, os.Mkdir(dir.Lease("zzz999"), 0o700), "create an orphan lease directory")
	setClock(t, expiry.Add(time.Minute))
	if ids := leaseIDs(t); !slices.Equal(ids, []string{id}) {
		t.Errorf("werft leases after the first expiry = %v, want the extended %s", ids, id)
	}
	if s, err := h.DomainState(name); err != nil || s != virt.StateRunning {
		t.Errorf("the extended %s is %v, %v, want running", name, s, err)
	}
	if _, err := os.Stat(dir.Lease("zzz999")); !os.IsNotExist(err) {
		t.Errorf("the orphan lease directory is left after the reap: %v", err)
	}

	setClock(t, extended.Add(time.Minute))
	if ids := leaseIDs(t); len(ids) != 0 {
		t.Errorf("werft leases after the new expiry = %v, want none", ids)
	}
	if _, err := h.Domain(name); !failure.Is(err, failure.NotFound) {
		t.Errorf("the expired %s is left: %v", name, err)
	}
	volumes, err := h.Volumes(livetest.Pool)
	livetest.Must(t, err, "list the volumes")
	if slices.Contains(volumes, name+".qcow2") {
		t.Errorf("the overlay of the expired %s is left", name)
	}
	if _, err := os.Stat(dir.Lease(id)); !os.IsNotExist(err) {
		t.Errorf("the lease directory of the expired lease is left: %v", err)
	}
}

// leaseIDs is the IDs werft --json leases lists.
func leaseIDs(t *testing.T) []string {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{stdout: &out, stderr: &errOut, dial: dialLibvirt, sleep: time.Sleep}
	if code := c.run([]string{"--json", "leases"}); code != 0 {
		t.Fatalf("werft leases exited %d: %s", code, errOut.String())
	}
	var leases []lease.Lease
	livetest.Must(t, json.Unmarshal(out.Bytes(), &leases), "parse the output of werft leases")
	ids := []string{}
	for _, l := range leases {
		ids = append(ids, l.ID)
	}
	return ids
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
