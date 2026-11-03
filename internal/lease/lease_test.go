package lease_test

import (
	"context"
	"encoding/base64"
	"errors"
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
