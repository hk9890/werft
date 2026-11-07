package lease_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// stockDomain is the definition seal keeps: the build VM on the stock
// volumes, with an installer ISO it leaves shared.
const stockDomain = `<domain type='kvm'><name>vm-plain-20261102</name><title>build VM</title><memory unit='MiB'>1024</memory><os firmware='efi'>
<type arch='x86_64' machine='q35'>hvm</type>
<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
<nvram format='qcow2'>/werft/vm-plain-20261102_VARS.qcow2</nvram></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/werft/vm-plain-20261102.qcow2'/><target dev='vda'/></disk>
<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><source file='/iso'/><target dev='sda'/><readonly/></disk>
<interface type='network'><mac address='52:54:00:00:00:01'/><source network='default'/></interface>
<vsock model='virtio'><cid auto='yes'/></vsock>
</devices></domain>`

func record() image.Record {
	return image.Record{
		Name: "vm-plain-20261102", Image: "vm", Tier: "plain", Version: "20261102", Source: "vm", Label: "provision-base",
		Pool: "werft", Disk: "vm-plain-20261102.qcow2", NVRAM: "vm-plain-20261102_VARS.qcow2",
		User: "hans", Family: "ubuntu", Systemd: 259, Domain: stockDomain,
	}
}

func world(t *testing.T) *virttest.Fake {
	t.Helper()
	f := virttest.New()
	must(t, virt.EnsurePool(f, "werft", "/werft"))
	_, err := f.CreateVolume("werft", "<volume><name>vm-plain-20261102.qcow2</name><capacity unit='G'>20</capacity></volume>")
	must(t, err)
	_, err = f.CreateVolume("werft", "<volume><name>vm-plain-20261102_VARS.qcow2</name><capacity>0</capacity></volume>")
	must(t, err)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// shell answers a login when up says so, and logs the ssh_config and
// address of each.
type shell struct {
	up     bool
	config string
	logins []string
}

func (s *shell) Run(_ context.Context, addr guest.Addr, _ string, _ io.Reader, _, _ io.Writer) error {
	s.logins = append(s.logins, addr.String())
	if !s.up {
		return errors.New("connection refused")
	}
	return nil
}

func env(t *testing.T, f *virttest.Fake, sh *shell) lease.Env {
	t.Helper()
	return lease.Env{
		Ctx: context.Background(), H: f, Dir: state.Dir(t.TempDir()), Prefix: "werft-",
		Shell: func(config string) guest.Shell { sh.config = config; return sh },
		NewKey: func(path string) error {
			if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
				return err
			}
			return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAAlease werft\n"), 0o600)
		},
		Global: func() (func(), error) { return func() {}, nil },
		Now:    func() time.Time { return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC) },
		Sleep:  func(time.Duration) {},
		Out:    io.Discard,
	}
}

func TestStart(t *testing.T) {
	f := world(t)
	sh := &shell{up: true}
	e := env(t, f, sh)
	l, err := lease.Start(e, record(), "agent")
	must(t, err)

	dir := e.Dir.Lease(l.ID)
	if l.Domain != "werft-vm-plain-"+l.ID || l.Stock != "vm-plain-20261102" || l.Owner != "agent" || l.User != "hans" ||
		l.Address != "vsock/3" || l.Key != filepath.Join(dir, "id_ed25519") || l.SSHConfig != filepath.Join(dir, "ssh_config") ||
		l.SSH != "ssh -F "+l.SSHConfig+" "+l.ID || l.Expiry != e.Now().Add(lease.Lifetime) {
		t.Errorf("lease = %+v", l)
	}
	if len(sh.logins) != 1 || sh.logins[0] != "hans@"+l.ID || sh.config != l.SSHConfig {
		t.Errorf("logins %v with %s, want hans@%s with %s", sh.logins, sh.config, l.ID, l.SSHConfig)
	}
	config, err := os.ReadFile(l.SSHConfig)
	must(t, err)
	for _, line := range []string{"Host " + l.ID, "HostName vsock/3", "User hans", "IdentityFile " + l.Key} {
		if !strings.Contains(string(config), line) {
			t.Errorf("ssh_config lacks %q:\n%s", line, config)
		}
	}
	if s, _ := f.DomainState(l.Domain); s != virt.StateRunning {
		t.Errorf("%s is %v, want running", l.Domain, s)
	}
	if meta, err := f.DomainMetadata(l.Domain, lease.Namespace); err != nil ||
		!strings.Contains(meta, "<werft:owner>agent</werft:owner>") || !strings.Contains(meta, "<werft:expiry>2026-11-03T14:00:00Z</werft:expiry>") ||
		!strings.Contains(meta, "<werft:stock>vm-plain-20261102</werft:stock>") || !strings.Contains(meta, "<werft:state>"+string(e.Dir)+"</werft:state>") {
		t.Errorf("metadata = %q, %v", meta, err)
	}

	text, err := f.DomainXML(l.Domain, true)
	must(t, err)
	d, err := domxml.Parse(text)
	must(t, err)
	if disks := domxml.Disks(d); len(disks) != 1 || disks[0].Source != "/werft/"+l.Domain+".qcow2" {
		t.Errorf("disks = %+v, want the overlay alone", disks)
	}
	if nv, _ := domxml.NVRAMOf(d); nv.Path != "" || nv.Template != "/werft/vm-plain-20261102_VARS.qcow2" || nv.TemplateFormat != "qcow2" || nv.Format != "qcow2" {
		t.Errorf("NVRAM = %+v, want a qcow2 one from the stock vars", nv)
	}
	if d.OS.Firmware != "" || d.Title != "" || strings.Contains(text, "52:54:00:00:00:01") {
		t.Errorf("the lease keeps firmware autoselection, the title or the MAC of the stock definition:\n%s", text)
	}
	creds := map[string]string{}
	for _, entry := range d.SysInfo[0].SMBIOS.OEMStrings.Entry {
		name, value, _ := strings.Cut(strings.TrimPrefix(entry, "io.systemd.credential.binary:"), "=")
		data, err := base64.StdEncoding.DecodeString(value)
		must(t, err)
		creds[name] = string(data)
	}
	if creds["system.hostname"] != l.Domain || len(creds["system.machine_id"]) != 32 ||
		creds["ssh.ephemeral-authorized_keys-all"] != "ssh-ed25519 AAAAlease werft\n" ||
		creds["werft.ssh.authorized_keys"] != "\nssh-ed25519 AAAAlease werft\n" ||
		!strings.Contains(creds["tmpfiles.extra"], "w+^ /home/hans/.ssh/authorized_keys - - - - werft.ssh.authorized_keys") ||
		creds["systemd.unit-dropin.systemd-tmpfiles-setup.service"] != "[Service]\nImportCredential=werft.*\n" {
		t.Errorf("credentials = %q", creds)
	}
}

// TestStartOverDHCP: a guest before systemd 256 has no SSH over vsock, so
// the lease waits for the DHCP lease of its MAC.
func TestStartOverDHCP(t *testing.T) {
	f := world(t)
	rec := record()
	rec.Systemd = 255
	e := env(t, f, &shell{up: true})
	started := false
	e.Sleep = func(time.Duration) {
		if !started {
			started = true
			names, _ := f.Domains()
			text, _ := f.DomainXML(names[0].Name, true)
			d, _ := domxml.Parse(text)
			f.AddDHCPLease("default", virt.DHCPLease{MAC: domxml.Interfaces(d)[0].MAC, IP: "192.168.122.40"})
		}
	}
	l, err := lease.Start(e, rec, "agent")
	must(t, err)
	if l.Address != "192.168.122.40" {
		t.Errorf("address = %s", l.Address)
	}
	config, _ := os.ReadFile(l.SSHConfig)
	if !strings.Contains(string(config), "HostName 192.168.122.40") || strings.Contains(string(config), "ProxyCommand") {
		t.Errorf("ssh_config:\n%s", config)
	}
}

func TestStartFailsAndReleases(t *testing.T) {
	tests := []struct {
		name string
		fail string
		up   bool
		want failure.Kind
	}{
		{"no SSH", "", false, failure.Timeout},
		{"start", "StartDomain", true, failure.Failed},
		{"define", "DefineDomain", true, failure.Conflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := world(t)
			if tt.fail != "" {
				f.FailOn(tt.fail, failure.New(tt.want, "injected"))
			}
			e := env(t, f, &shell{up: tt.up})
			if _, err := lease.Start(e, record(), "agent"); !failure.Is(err, tt.want) {
				t.Fatalf("Start = %v, want kind %s", err, tt.want)
			}
			if domains, _ := f.Domains(); len(domains) != 0 {
				t.Errorf("domains left: %v", domains)
			}
			if volumes, _ := f.Volumes("werft"); len(volumes) != 2 {
				t.Errorf("volumes left: %v", volumes)
			}
			if entries, _ := os.ReadDir(e.Dir.Leases()); len(entries) != 0 {
				t.Errorf("lease directories left: %v", entries)
			}
		})
	}
}

// domainOf is a domain definition with mem of memory, with werft lease
// metadata when lease is set.
func domainOf(name, mem string, lease bool) string {
	meta := ""
	if lease {
		meta = "<metadata><werft:lease xmlns:werft='https://github.com/hk9890/werft/lease'><werft:owner>other</werft:owner></werft:lease></metadata>"
	}
	return "<domain type='kvm'><name>" + name + "</name><memory unit='MiB'>" + mem + "</memory>" + meta + "</domain>"
}

// TestStartAdmits: a lease that would take the host memory, less the memory
// of every running domain and every lease domain, below the budget fails
// with no_capacity and changes nothing; so does a lease on a full pool.
// Controls: a shut-off domain that is no lease holds no memory, and a lease
// that leaves exactly the budget starts. The checks and the define run
// under the global lock.
func TestStartAdmits(t *testing.T) {
	const budget = 4 << 30
	const need = 1 << 30 // the memory of stockDomain
	define := func(name, mem string, lease, start bool) func(*testing.T, *virttest.Fake) {
		return func(t *testing.T, f *virttest.Fake) {
			_, err := f.DefineDomain(domainOf(name, mem, lease))
			must(t, err)
			if start {
				must(t, f.StartDomain(name))
			}
		}
	}
	tests := []struct {
		name      string
		total     uint64
		pool      uint64
		setup     func(*testing.T, *virttest.Fake)
		wantError bool
	}{
		{"memory below the budget", budget + need - 1, 100 << 30, nil, true},
		{"less memory than the lease", need - 1, 100 << 30, nil, true},
		{"a running domain counts", budget + need + 2<<30 - 1, 100 << 30, define("build", "2048", false, true), true},
		{"a lease defined but not started counts", budget + need + 2<<30 - 1, 100 << 30, define("werft-vm-plain-other1", "2048", true, false), true},
		{"pool full", budget + need, lease.MinPoolFree - 1, nil, true},
		{"control: a shut-off domain holds nothing", budget + need, 100 << 30, define("build", "2048", false, false), false},
		{"control: exactly the budget left", budget + need, 100 << 30, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := world(t)
			f.TotalMemory = tt.total
			f.SetPoolCapacity("werft", tt.pool)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			e := env(t, f, &shell{up: true})
			e.MemoryBudget = budget
			var locked []string
			e.Global = func() (func(), error) {
				start := len(f.Calls())
				return func() { locked = f.Calls()[start:] }, nil
			}
			before := len(f.Calls())
			_, err := lease.Start(e, record(), "agent")
			if !tt.wantError {
				must(t, err)
				for _, call := range []string{"HostTotalMemory", "Domains", "PoolInfo werft", "DefineDomain"} {
					if !slices.ContainsFunc(locked, func(c string) bool { return strings.HasPrefix(c, call) }) {
						t.Errorf("%s ran outside the global lock; under it: %v", call, locked)
					}
				}
				return
			}
			if !failure.Is(err, failure.NoCapacity) || !strings.Contains(err.Error(), "no capacity") {
				t.Fatalf("Start = %v, want kind no_capacity", err)
			}
			if calls := f.Calls()[before:]; slices.ContainsFunc(calls, func(c string) bool {
				return strings.HasPrefix(c, "CreateVolume") || strings.HasPrefix(c, "DefineDomain")
			}) {
				t.Errorf("a refused lease made a volume or a domain: %v", calls)
			}
			if _, err := os.Stat(e.Dir.Leases()); !os.IsNotExist(err) {
				t.Errorf("a refused lease made a lease directory: %v", err)
			}
		})
	}
}

// TestStartAdmitsInSequence: the memory of an admitted lease counts for the
// next admission, though the guest has not touched it yet. With room for one
// lease above the budget, the first lease starts and the second fails with
// no_capacity; the message names what helps.
func TestStartAdmitsInSequence(t *testing.T) {
	f := world(t)
	f.TotalMemory = 4<<30 + 1<<30 + 1<<29
	e := env(t, f, &shell{up: true})
	e.MemoryBudget = 4 << 30
	_, err := lease.Start(e, record(), "agent")
	must(t, err)
	_, err = lease.Start(e, record(), "agent")
	if !failure.Is(err, failure.NoCapacity) {
		t.Fatalf("second Start = %v, want kind no_capacity", err)
	}
	for _, want := range []string{"host has 5632 MiB", "domains hold 1024 MiB", "lease needs 1024 MiB", "keeps 4096 MiB", "release a lease, stop a VM, or lower memory_budget_mib"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no_capacity message lacks %q: %v", want, err)
		}
	}
}

func TestStartRefusesARecordWithoutDomain(t *testing.T) {
	f := world(t)
	rec := record()
	rec.Domain = ""
	if _, err := lease.Start(env(t, f, &shell{up: true}), rec, "agent"); !failure.Is(err, failure.Unsupported) {
		t.Errorf("Start = %v, want unsupported", err)
	}
}

func TestResolve(t *testing.T) {
	catalog := filepath.Join(t.TempDir(), "images.toml")
	must(t, os.WriteFile(catalog, []byte(`[[image]]
name = "vm-plain-20261101"
image = "vm"
tier = "plain"
[[image]]
name = "vm-plain-20261102"
image = "vm"
tier = "plain"
[[image]]
name = "other-plain-20261103"
image = "other"
tier = "plain"
`), 0o600))
	if r, err := lease.Resolve(catalog, "vm", "plain"); err != nil || r.Name != "vm-plain-20261102" {
		t.Errorf("Resolve = %s, %v, want the newest version", r.Name, err)
	}
	if _, err := lease.Resolve(catalog, "vm", "full"); !failure.Is(err, failure.NotFound) {
		t.Errorf("Resolve of a missing tier = %v, want not_found", err)
	}
}

func TestRelease(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l, err := lease.Start(e, record(), "agent")
	must(t, err)
	// A domain with a lease name but no lease metadata is no lease.
	_, err = f.DefineDomain("<domain type='kvm'><name>werft-other-plain-" + l.ID + "</name></domain>")
	must(t, err)

	name, err := lease.Release(f, e.Dir, "werft-", l.ID)
	if err != nil || name != l.Domain {
		t.Fatalf("Release = %s, %v", name, err)
	}
	if _, err := f.Domain(l.Domain); !failure.Is(err, failure.NotFound) {
		t.Errorf("%s is left: %v", l.Domain, err)
	}
	if _, err := f.Domain("werft-other-plain-" + l.ID); err != nil {
		t.Errorf("Release removed a domain without lease metadata: %v", err)
	}
	if volumes, _ := f.Volumes("werft"); slices.Contains(volumes, l.Domain+".qcow2") {
		t.Errorf("the overlay is left: %v", volumes)
	}
	if _, err := os.Stat(e.Dir.Lease(l.ID)); !os.IsNotExist(err) {
		t.Errorf("the lease directory is left: %v", err)
	}
	if name, err := lease.Release(f, e.Dir, "werft-", l.ID); err != nil || name != "" {
		t.Errorf("second Release = %q, %v", name, err)
	}
	if _, err := lease.Release(f, e.Dir, "werft-", "../.."); !failure.Is(err, failure.Usage) {
		t.Errorf("Release of a path = %v, want usage", err)
	}
}

// TestReleaseFinishesAHalfDoneRelease: a release that stopped after the
// overlay went finds the domain again and ends the work.
func TestReleaseFinishesAHalfDoneRelease(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l, err := lease.Start(e, record(), "agent")
	must(t, err)
	f.FailOn("UndefineDomain", failure.New(failure.Failed, "injected"))
	if _, err := lease.Release(f, e.Dir, "werft-", l.ID); err == nil {
		t.Fatal("Release with a failing undefine succeeded")
	}
	f.FailOn("UndefineDomain", nil)
	if name, err := lease.Release(f, e.Dir, "werft-", l.ID); err != nil || name != l.Domain {
		t.Fatalf("second Release = %s, %v", name, err)
	}
	if _, err := f.Domain(l.Domain); !failure.Is(err, failure.NotFound) {
		t.Errorf("%s is left: %v", l.Domain, err)
	}
}

// screen is the fake whose every console shows a still prompt.
type screen struct{ *virttest.Fake }

func (s screen) Screenshot(domain string, w io.Writer) (string, error) {
	if _, err := s.Fake.Screenshot(domain, io.Discard); err != nil {
		return "", err
	}
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.White)
	return "image/png", png.Encode(w, img)
}

// store is pass with one entry.
type store map[string]string

func (s store) Show(entry string) (string, error) {
	if v, ok := s[entry]; ok {
		return v, nil
	}
	return "", failure.New(failure.SecretUnavailable, "pass show %s failed", entry)
}

// noTerminal is stdin that is no terminal, as for an agent.
type noTerminal struct{}

func (noTerminal) ReadSecret(prompt string) (string, error) {
	return "", failure.New(failure.SecretUnavailable, "%s: stdin is no terminal to ask on", prompt)
}

func luksRecord() image.Record {
	rec := record()
	rec.LUKS, rec.DiskPasswordPass = true, "vm/guest-password"
	return rec
}

// keyShell is a guest that answers SSH only once keys were typed: an
// encrypted guest takes no address before its disk is open.
type keyShell struct {
	shell
	f *virttest.Fake
}

func (s *keyShell) Run(ctx context.Context, addr guest.Addr, cmd string, in io.Reader, stdout, stderr io.Writer) error {
	s.up = slices.ContainsFunc(s.f.Calls(), func(c string) bool { return strings.HasPrefix(c, "SendKey ") })
	return s.shell.Run(ctx, addr, cmd, in, stdout, stderr)
}

func TestStartTypesTheDiskPasswordOnce(t *testing.T) {
	f := world(t)
	sh := &keyShell{f: f}
	e := env(t, f, &sh.shell)
	e.Shell = func(string) guest.Shell { return sh }
	e.H = screen{f}
	var out bytes.Buffer
	e.Out = &out
	e.Secrets, e.Terminal = store{"vm/guest-password": "pw"}, noTerminal{}
	l, err := lease.Start(e, luksRecord(), "agent")
	must(t, err)

	// p, w, Enter: once.
	if keys := f.SentKeys(l.Domain); len(keys) != 3 || keys[2][0] != 28 {
		t.Errorf("keys = %v, want the password and Enter once", keys)
	}
	if len(sh.logins) != 1 {
		t.Errorf("logins = %v, want one after the password", sh.logins)
	}
	doc, err := json.Marshal(l)
	must(t, err)
	if strings.Contains(string(doc), `"pw"`) || strings.Contains(out.String(), "pw\n") {
		t.Errorf("the password shows in the JSON or the output:\n%s\n%s", doc, out.String())
	}
}

func TestStartWithoutTheDiskPassword(t *testing.T) {
	tests := map[string]image.Record{
		"pass cannot give it":     luksRecord(),
		"the record has no entry": func() image.Record { r := luksRecord(); r.DiskPasswordPass = ""; return r }(),
	}
	for name, rec := range tests {
		t.Run(name, func(t *testing.T) {
			f := world(t)
			e := env(t, f, &shell{up: true})
			e.Secrets, e.Terminal = store{}, noTerminal{}
			before := len(f.Calls())
			if _, err := lease.Start(e, rec, "agent"); !failure.Is(err, failure.SecretUnavailable) {
				t.Fatalf("Start = %v, want secret_unavailable", err)
			}
			for _, c := range f.Calls()[before:] {
				t.Errorf("Start without the password called libvirt: %s", c)
			}
			if _, err := os.Stat(e.Dir.Leases()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Start without the password made %s: %v", e.Dir.Leases(), err)
			}
		})
	}
}

func TestStartWithALockedPasswordStoreTellsWhatToDo(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	e.Secrets, e.Terminal = store{}, noTerminal{}
	_, err := lease.Start(e, luksRecord(), "agent")
	if !failure.Is(err, failure.SecretUnavailable) {
		t.Fatalf("Start = %v, want secret_unavailable", err)
	}
	for _, want := range []string{"ask the user to unlock the password store", "pass show vm/guest-password once", "then lease again", "do not print or ask for the password yourself"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to say %q", err, want)
		}
	}
}

func TestStartRefusesAnUntypeablePassword(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	e.Secrets, e.Terminal = store{"vm/guest-password": "pässword"}, noTerminal{}
	before := len(f.Calls())
	if _, err := lease.Start(e, luksRecord(), "agent"); !failure.Is(err, failure.Usage) || strings.Contains(err.Error(), "pässword") {
		t.Fatalf("Start = %v, want usage without the password", err)
	}
	for _, c := range f.Calls()[before:] {
		t.Errorf("Start with an untypeable password called libvirt: %s", c)
	}
	if _, err := os.Stat(e.Dir.Leases()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Start with an untypeable password made %s: %v", e.Dir.Leases(), err)
	}
}

// TestStartOfARefusedPasswordNamesNoRemovedDomain: the failed start removes
// the lease domain, so the error must not send the user to its console.
func TestStartOfARefusedPasswordNamesNoRemovedDomain(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{})
	e.H = screen{f}
	e.Secrets, e.Terminal = store{"vm/guest-password": "pw"}, noTerminal{}
	_, err := lease.Start(e, luksRecord(), "agent")
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "refused the typed password") {
		t.Fatalf("Start = %v, want a timeout that names the password", err)
	}
	if strings.Contains(err.Error(), "virt-viewer") {
		t.Errorf("Start names the console of the removed domain: %v", err)
	}
}

// TestStartOfAPlainImageTypesNothing: an image without the LUKS marker
// reads no secret.
func TestStartOfAPlainImageTypesNothing(t *testing.T) {
	f := world(t)
	e := env(t, f, &shell{up: true})
	l, err := lease.Start(e, record(), "agent")
	must(t, err)
	if keys := f.SentKeys(l.Domain); len(keys) != 0 {
		t.Errorf("keys = %v, want none", keys)
	}
}
