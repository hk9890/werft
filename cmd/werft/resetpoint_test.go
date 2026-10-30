package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// rpVM is a build VM with qcow2 disk and NVRAM, which takes native reset
// points; rawVM is one that vm manages, with its NVRAM in nvramDir.
const rpVM = `<domain type='kvm'><name>vm</name><os><type>hvm</type>
<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
<nvram format='qcow2'>/nvram/vm_VARS.qcow2</nvram></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm.qcow2'/><target dev='vda'/></disk>
</devices></domain>`

type rpHost struct {
	t    *testing.T
	fake *virttest.Fake
}

func newRPHost(t *testing.T) *rpHost {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := virttest.New()
	if _, err := f.DefineDomain(rpVM); err != nil {
		t.Fatal(err)
	}
	return &rpHost{t: t, fake: f}
}

func (rh *rpHost) run(stdin string, tty bool, args ...string) result {
	var out, errOut bytes.Buffer
	c := &cli{
		stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut, tty: tty,
		dial: func() (virt.Hypervisor, error) { return rh.fake, nil },
	}
	code := c.run(args)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func (rh *rpHost) snapshots() []string {
	rh.t.Helper()
	names, err := rh.fake.Snapshots("vm")
	if err != nil {
		rh.t.Fatal(err)
	}
	return names
}

func (rh *rpHost) reverts() int {
	n := 0
	for _, call := range rh.fake.Calls() {
		if strings.HasPrefix(call, "RevertSnapshot ") {
			n++
		}
	}
	return n
}

func TestSnapshot(t *testing.T) {
	rh := newRPHost(t)
	r := rh.run("", false, "snapshot", "vm")
	if r.code != 0 || r.stdout != "vm reset point 'clean-install' created\n  reset with: werft reset vm clean-install\n" {
		t.Errorf("snapshot = %+v", r)
	}
	r = rh.run("", false, "snapshot", "vm")
	if r.code != 4 || !strings.Contains(r.stderr, "already has a reset point 'clean-install'") || !strings.Contains(r.stderr, "--replace") {
		t.Errorf("second snapshot = %+v", r)
	}
	r = rh.run("", false, "--json", "snapshot", "--replace", "vm")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || r.code != 0 || doc["replaced"] != true || doc["reset_point"] != "clean-install" {
		t.Errorf("snapshot --replace = %+v", r)
	}
	if r := rh.run("", false, "snapshot", "vm", "base"); r.code != 0 {
		t.Errorf("snapshot with a label = %+v", r)
	}
	if got := rh.snapshots(); !slices.Equal(got, []string{"clean-install", "base"}) {
		t.Errorf("snapshots = %q", got)
	}
}

func TestSnapshotRefusals(t *testing.T) {
	rh := newRPHost(t)
	dir := t.TempDir()
	raw := strings.NewReplacer("<name>vm</name>", "<name>raw</name>", "format='qcow2'>/nvram/vm_VARS.qcow2", "format='raw'>"+dir+"/raw_VARS.fd").Replace(rpVM)
	if _, err := rh.fake.DefineDomain(raw); err != nil {
		t.Fatal(err)
	}
	if err := rh.fake.StartDomain("vm"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"snapshot", "vm"}, 4, "vm is running; a reset point needs it shut off"},
		{[]string{"snapshot", "raw"}, 9, "run werft migrate raw"},
		{[]string{"snapshot", "none"}, 3, "unknown VM none"},
		{[]string{"snapshot"}, 2, "snapshot takes a VM name"},
		{[]string{"snapshot", "vm", "a", "b"}, 2, "snapshot takes a VM name"},
		{[]string{"reset", "--yes", "vm"}, 4, "vm is running; a reset needs it shut off"},
		{[]string{"reset", "--yes", "raw"}, 9, "run werft migrate raw"},
		{[]string{"reset", "--yes", "none"}, 3, "unknown VM none"},
		{[]string{"snapshots"}, 2, "snapshots takes one VM name"},
		{[]string{"snapshots", "none"}, 3, "unknown VM none"},
	}
	for _, tt := range tests {
		if r := rh.run("", false, tt.args...); r.code != tt.code || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("%q = %+v, want exit %d and %q", tt.args, r, tt.code, tt.want)
		}
	}
	if calls := strings.Join(rh.fake.Calls(), "\n"); strings.Contains(calls, "CreateSnapshot") || strings.Contains(calls, "RevertSnapshot") {
		t.Errorf("a refusal changed a reset point:\n%s", calls)
	}
}

func TestSnapshots(t *testing.T) {
	rh := newRPHost(t)
	if r := rh.run("", false, "snapshots", "vm"); r.code != 0 || r.stdout != "vm has no reset points\n" {
		t.Errorf("snapshots without any = %+v", r)
	}
	for _, label := range []string{"clean-install", "provision-base"} {
		if err := rh.fake.CreateSnapshot("vm", label, ""); err != nil {
			t.Fatal(err)
		}
	}
	if r := rh.run("", false, "snapshots", "vm"); r.code != 0 || r.stdout != "clean-install\nprovision-base\n" {
		t.Errorf("snapshots = %+v", r)
	}

	dir := t.TempDir()
	raw := strings.NewReplacer("<name>vm</name>", "<name>raw</name>", "format='qcow2'>/nvram/vm_VARS.qcow2", "format='raw'>"+dir+"/raw_VARS.fd").Replace(rpVM)
	if _, err := rh.fake.DefineDomain(raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "raw_VARS.fd.provision-base.bak"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := rh.run("", false, "snapshots", "raw")
	if r.code != 0 || r.stdout != "reset points of vm, which werft migrate raw moves to werft:\n  provision-base\n" {
		t.Errorf("snapshots of a legacy domain = %+v", r)
	}
	r = rh.run("", false, "--json", "snapshots", "raw")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	want := map[string]any{"domain": "raw", "reset_points": []any{}, "legacy": []any{"provision-base"}}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("snapshots --json = %v, want %v", doc, want)
	}
}

func TestReset(t *testing.T) {
	rh := newRPHost(t)
	if err := rh.fake.CreateSnapshot("vm", "clean-install", ""); err != nil {
		t.Fatal(err)
	}
	r := rh.run("", false, "reset", "vm")
	if r.code != 5 || !strings.Contains(r.stderr, "--yes") || rh.reverts() != 0 {
		t.Errorf("reset without a terminal = %+v", r)
	}
	if r := rh.run("n\n", true, "reset", "vm"); r.code != 5 || rh.reverts() != 0 {
		t.Errorf("reset answered no = %+v", r)
	}
	r = rh.run("y\n", true, "reset", "vm")
	if r.code != 0 || r.stdout != "vm is back at 'clean-install'\n" || !strings.Contains(r.stderr, "Everything written since is lost") || rh.reverts() != 1 {
		t.Errorf("reset answered yes = %+v", r)
	}
	if r := rh.run("", false, "reset", "-y", "vm"); r.code != 0 || rh.reverts() != 2 {
		t.Errorf("reset -y = %+v", r)
	}
	r = rh.run("", false, "reset", "vm", "none")
	if r.code != 3 || !strings.Contains(r.stderr, "werft snapshots vm") {
		t.Errorf("reset to a missing label = %+v; it must be refused before the question", r)
	}
}

func TestResetPointCommandsTakeTheDomainLock(t *testing.T) {
	rh := newRPHost(t)
	dir, err := state.Root()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := dir.LockDomain("vm")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	for _, args := range [][]string{{"snapshot", "vm"}, {"reset", "--yes", "vm"}, {"migrate", "--yes", "vm"}} {
		if r := rh.run("", false, args...); r.code != 4 || !strings.Contains(r.stderr, "another werft run works on vm") {
			t.Errorf("%q while another run holds the lock = %+v", args, r)
		}
	}
	if r := rh.run("", false, "snapshots", "vm"); r.code != 0 {
		t.Errorf("snapshots, which only reads, = %+v", r)
	}
}

func TestMigrateCommand(t *testing.T) {
	rh := newRPHost(t)
	for pool, path := range map[string]string{"default": "/img", "nvram": "/nvram", "mypool": "/img/mypool"} {
		if err := virt.EnsurePool(rh.fake, pool, path); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range [][2]string{{"default", "b.qcow2"}, {"nvram", "b_VARS.fd"}, {"nvram", "b_VARS.fd.clean-install.bak"}, {"nvram", "b_VARS.fd.provision-base.bak"}} {
		if _, err := rh.fake.CreateVolume(v[0], "<volume><name>"+v[1]+"</name><capacity>1</capacity></volume>"); err != nil {
			t.Fatal(err)
		}
	}
	for _, label := range []string{"clean-install", "provision-base"} {
		if err := rh.fake.QemuImgSnapshot("default", "b.qcow2", label, label); err != nil {
			t.Fatal(err)
		}
	}
	legacy := strings.NewReplacer("<name>vm</name>", "<name>b</name>", "format='qcow2'>/nvram/vm_VARS.qcow2", "format='raw'>/nvram/b_VARS.fd", "/img/vm.qcow2", "/img/b.qcow2").Replace(rpVM)
	if _, err := rh.fake.DefineDomain(legacy); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "werft", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("pool = \"mypool\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := rh.run("", false, "migrate", "b")
	if r.code != 5 || !strings.Contains(r.stderr, "'clean-install' moves to b-clean-install.qcow2 and b-clean-install_VARS.qcow2 in pool mypool") {
		t.Errorf("migrate without a terminal = %+v", r)
	}
	if names, _ := rh.fake.Snapshots("b"); len(names) != 0 {
		t.Errorf("an unconfirmed migrate made snapshots %q", names)
	}

	r = rh.run("", false, "--json", "migrate", "--yes", "b")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %+v", r)
	}
	want := map[string]any{"domain": "b", "kept": "provision-base", "export_pool": "mypool",
		"exported": []any{"b-clean-install.qcow2", "b-clean-install_VARS.qcow2"}, "deleted": []any{}}
	if r.code != 0 || !reflect.DeepEqual(doc, want) {
		t.Errorf("migrate --json = %v, %+v", doc, r)
	}
	if names, _ := rh.fake.Snapshots("b"); !slices.Equal(names, []string{"provision-base"}) {
		t.Errorf("snapshots after migrate = %q", names)
	}
	if r := rh.run("", false, "migrate", "--yes", "b"); r.code != 4 || !strings.Contains(r.stderr, "nothing to migrate") {
		t.Errorf("second migrate = %+v", r)
	}
	if r := rh.run("", false, "migrate"); r.code != 2 {
		t.Errorf("migrate without a name = %+v", r)
	}
}
