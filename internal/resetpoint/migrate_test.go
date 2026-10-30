package resetpoint_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// legacyVM is a build VM as vm leaves it: firmware autoselection with the
// loader libvirt picked, a raw NVRAM in the nvram pool, a qcow2 disk in the
// default pool, and an installer ISO.
func legacyVM(name string) string {
	return `<domain type='kvm'><name>` + name + `</name><os firmware='efi'>
<type arch='x86_64' machine='q35'>hvm</type>
<firmware><feature enabled='yes' name='secure-boot'/></firmware>
<loader readonly='yes' secure='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.ms.fd</loader>
<nvram template='/usr/share/OVMF/OVMF_VARS_4M.ms.fd' templateFormat='raw' format='raw'>/nvram/` + name + `_VARS.fd</nvram></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/` + name + `.qcow2'/><target dev='vda'/></disk>
<disk type='file' device='cdrom'><driver name='qemu' type='raw'/><source file='/iso'/><target dev='sda'/><readonly/></disk>
</devices></domain>`
}

type legacyHost struct {
	t *testing.T
	f *virttest.Fake
}

// newLegacyHost has the build VM vm with the reset points of vm in labels,
// each qemu-img snapshot and NVRAM copy holding its label, and the export
// pool werft.
func newLegacyHost(t *testing.T, name string, labels ...string) *legacyHost {
	t.Helper()
	lh := &legacyHost{t: t, f: virttest.New()}
	for pool, path := range map[string]string{"default": "/img", "nvram": "/nvram", "werft": "/werft"} {
		lh.must(virt.EnsurePool(lh.f, pool, path))
	}
	lh.volume("default", name+".qcow2", "disk now")
	lh.volume("nvram", name+"_VARS.fd", "vars now")
	for _, label := range labels {
		lh.volume("nvram", name+"_VARS.fd."+label+".bak", "vars "+label)
		lh.must(lh.f.QemuImgSnapshot("default", name+".qcow2", label, "disk "+label))
	}
	_, err := lh.f.DefineDomain(legacyVM(name))
	lh.must(err)
	return lh
}

func (lh *legacyHost) must(err error) {
	lh.t.Helper()
	if err != nil {
		lh.t.Fatal(err)
	}
}

func (lh *legacyHost) volume(pool, name, data string) {
	lh.t.Helper()
	_, err := lh.f.CreateVolume(pool, "<volume><name>"+name+"</name><capacity unit='G'>20</capacity></volume>")
	lh.must(err)
	lh.must(lh.f.UploadVolume(pool, name, strings.NewReader(data), uint64(len(data))))
}

func (lh *legacyHost) migrate(domain string) (*resetpoint.Migration, error) {
	m, err := resetpoint.PlanMigration(lh.f, domain, "werft")
	if err != nil {
		return nil, err
	}
	return m, m.Run(lh.f, func(string, ...any) {})
}

func (lh *legacyHost) volumes(pool string) []string {
	lh.t.Helper()
	names, err := lh.f.Volumes(pool)
	lh.must(err)
	return names
}

func (lh *legacyHost) data(pool, name string) string {
	d, _ := lh.f.VolumeData(pool, name)
	return string(d)
}

// assertMigrated checks the end state of a migration of vm that kept
// provision-base and exported clean-install.
func (lh *legacyHost) assertMigrated() {
	t := lh.t
	t.Helper()
	if names, err := lh.f.Snapshots("vm"); err != nil || !slices.Equal(names, []string{"provision-base"}) {
		t.Errorf("snapshots = %q, %v", names, err)
	}
	if err := resetpoint.Guard(lh.f, "vm"); err != nil {
		t.Errorf("guard after migration: %v", err)
	}
	text, err := lh.f.DomainXML("vm", true)
	lh.must(err)
	d, err := domxml.Parse(text)
	lh.must(err)
	nv, _ := domxml.NVRAMOf(d)
	l, _ := domxml.LoaderOf(d)
	if nv != (domxml.NVRAM{Path: "/nvram/vm_VARS.qcow2", Format: "qcow2"}) || l.Path != "/usr/share/OVMF/OVMF_CODE_4M.ms.fd" || d.OS.Firmware != "" {
		t.Errorf("domain NVRAM %+v, loader %+v, firmware %q", nv, l, d.OS.Firmware)
	}
	snap, err := lh.f.SnapshotXML("vm", "provision-base")
	lh.must(err)
	if !strings.Contains(snap, "/nvram/vm_VARS.qcow2") {
		t.Errorf("the native reset point is not of the qcow2 NVRAM:\n%s", snap)
	}
	if got := lh.volumes("nvram"); !slices.Equal(got, []string{"vm_VARS.qcow2"}) {
		t.Errorf("nvram pool = %q, want only the qcow2 NVRAM", got)
	}
	if got := lh.data("nvram", "vm_VARS.qcow2"); got != "vars provision-base" {
		t.Errorf("qcow2 NVRAM holds %q, want the copy of provision-base", got)
	}
	if got := lh.data("default", "vm.qcow2"); got != "disk provision-base" {
		t.Errorf("disk holds %q, want provision-base", got)
	}
	if got := lh.volumes("werft"); !slices.Equal(got, []string{"vm-clean-install.qcow2", "vm-clean-install_VARS.qcow2"}) {
		t.Errorf("werft pool = %q", got)
	}
	if got := lh.data("werft", "vm-clean-install_VARS.qcow2"); got != "vars clean-install" {
		t.Errorf("exported NVRAM holds %q", got)
	}
	if got := lh.data("werft", "vm-clean-install.qcow2"); got != "disk clean-install" {
		t.Errorf("exported disk holds %q", got)
	}
}

func TestMigrate(t *testing.T) {
	lh := newLegacyHost(t, "vm", "clean-install", "full", "provision-base")
	since := len(lh.f.Calls())
	m, err := lh.migrate("vm")
	if err != nil {
		t.Fatal(err)
	}
	if m.Keep != "provision-base" || !slices.Equal(m.Delete, []string{"full"}) ||
		!slices.Equal(m.Export, []string{"vm-clean-install.qcow2", "vm-clean-install_VARS.qcow2"}) || m.Resume {
		t.Errorf("migration = %+v", m)
	}
	lh.assertMigrated()

	calls := lh.f.Calls()[since:]
	order := []string{
		"RedefineSnapshot vm provision-base",
		"RevertSnapshot vm provision-base",
		"RedefineSnapshot vm clean-install",
		"RevertSnapshot vm clean-install",
		"CreateVolumeFrom werft vm-clean-install.qcow2 default vm.qcow2",
		"CreateVolumeFrom werft vm-clean-install_VARS.qcow2 nvram vm_VARS.fd.clean-install.bak",
		"DeleteVolume nvram vm_VARS.fd.clean-install.bak",
		"DeleteSnapshot vm clean-install",
		"DeleteVolume nvram vm_VARS.fd.full.bak",
		"DeleteSnapshot vm full",
		"RevertSnapshot vm provision-base",
		"CreateVolumeFrom nvram vm_VARS.qcow2 nvram vm_VARS.fd.provision-base.bak",
		"DefineDomain vm",
		"DeleteVolume nvram vm_VARS.fd",
		"DeleteSnapshot vm provision-base",
		"CreateSnapshot vm provision-base",
	}
	rest := calls
	for _, call := range order {
		i := slices.Index(rest, call)
		if i < 0 {
			t.Fatalf("%q is missing or out of order in %q", call, calls)
		}
		rest = rest[i+1:]
	}

	if _, err := lh.migrate("vm"); !failure.Is(err, failure.Conflict) {
		t.Errorf("second migration = %v, want conflict", err)
	}
}

// TestMigrateWithoutTheKeptLabel migrates a domain that vm never prepared
// and never snapshotted: it keeps its current disk and NVRAM, and gets no
// reset point.
func TestMigrateWithoutTheKeptLabel(t *testing.T) {
	lh := newLegacyHost(t, "vm")
	since := len(lh.f.Calls())
	m, err := lh.migrate("vm")
	if err != nil {
		t.Fatal(err)
	}
	if m.Keep != "" || m.Export != nil || m.Delete != nil {
		t.Errorf("migration = %+v", m)
	}
	if names, _ := lh.f.Snapshots("vm"); len(names) != 0 {
		t.Errorf("snapshots = %q, want none", names)
	}
	if got := lh.volumes("nvram"); !slices.Equal(got, []string{"vm_VARS.qcow2"}) || lh.data("nvram", "vm_VARS.qcow2") != "vars now" {
		t.Errorf("nvram pool = %q, want the current NVRAM in qcow2", got)
	}
	if got := lh.data("default", "vm.qcow2"); got != "disk now" {
		t.Errorf("disk holds %q, want its current state", got)
	}
	if err := resetpoint.Guard(lh.f, "vm"); err != nil {
		t.Errorf("guard after migration: %v", err)
	}
	for _, call := range lh.f.Calls()[since:] {
		if strings.HasPrefix(call, "RevertSnapshot ") {
			t.Errorf("migration without a reset point called %q", call)
		}
	}

	lh = newLegacyHost(t, "vm")
	lh.f.FailOn("DeleteVolume nvram vm_VARS.fd", errors.New("boom"))
	if _, err := lh.migrate("vm"); err == nil || !strings.Contains(err.Error(), "vol-delete --pool nvram vm_VARS.fd") {
		t.Errorf("migrate with a failing delete of the old NVRAM = %v", err)
	}
}

// TestMigrateKeepsCleanInstallWithoutTheKeptLabel migrates a vendor install
// that vm snapshotted only as clean-install: clean-install becomes the
// native reset point, disk and NVRAM both at clean-install.
func TestMigrateKeepsCleanInstallWithoutTheKeptLabel(t *testing.T) {
	for _, fail := range []string{"", "DeleteVolume nvram vm_VARS.fd", "DeleteSnapshot vm clean-install"} {
		t.Run("fail "+fail, func(t *testing.T) {
			lh := newLegacyHost(t, "vm", "clean-install", "full")
			if fail != "" {
				lh.f.FailOn(fail, errors.New("boom"))
				if _, err := lh.migrate("vm"); err == nil {
					t.Fatalf("migrate with %s failing passed", fail)
				}
				lh.f.FailOn(fail, nil)
			}
			m, err := lh.migrate("vm")
			if err != nil {
				t.Fatal(err)
			}
			if fail == "" && (m.Keep != "clean-install" || m.Export != nil || !slices.Equal(m.Delete, []string{"full"})) {
				t.Errorf("migration = %+v", m)
			}
			if names, _ := lh.f.Snapshots("vm"); !slices.Equal(names, []string{"clean-install"}) {
				t.Errorf("snapshots = %q", names)
			}
			if got := lh.volumes("nvram"); !slices.Equal(got, []string{"vm_VARS.qcow2"}) || lh.data("nvram", "vm_VARS.qcow2") != "vars clean-install" {
				t.Errorf("nvram pool = %q, want the copy of clean-install in qcow2", got)
			}
			if got := lh.data("default", "vm.qcow2"); got != "disk clean-install" {
				t.Errorf("disk holds %q, want clean-install", got)
			}
			if got := lh.volumes("werft"); len(got) != 0 {
				t.Errorf("werft pool = %q, want nothing exported", got)
			}
		})
	}
}

// TestMigrateChecksTheKeptLabelFirst migrates a domain whose disk lacks the
// qemu-img snapshot of the kept label, and checks that nothing is deleted.
func TestMigrateChecksTheKeptLabelFirst(t *testing.T) {
	lh := newLegacyHost(t, "vm", "clean-install", "full")
	lh.volume("nvram", "vm_VARS.fd.provision-base.bak", "vars provision-base")
	since := len(lh.f.Calls())
	if _, err := lh.migrate("vm"); err == nil || !strings.Contains(err.Error(), "'provision-base'") {
		t.Fatalf("migrate = %v", err)
	}
	for _, call := range lh.f.Calls()[since:] {
		if method := strings.Fields(call)[0]; method == "DeleteSnapshot" || method == "DeleteVolume" || method == "CreateVolumeFrom" {
			t.Errorf("a migration that cannot restore the kept label called %q", call)
		}
	}
	if got := lh.data("default", "vm.qcow2"); got != "disk now" {
		t.Errorf("disk holds %q, want its current state", got)
	}
}

// TestMigrateRefusesAForeignExport finds an export volume that no run of
// this migration made, for example from a domain of the same name that was
// built again since.
func TestMigrateRefusesAForeignExport(t *testing.T) {
	for _, name := range []string{"vm-clean-install.qcow2", "vm-clean-install_VARS.qcow2"} {
		lh := newLegacyHost(t, "vm", "clean-install", "provision-base")
		lh.volume("werft", name, "old")
		since := len(lh.f.Calls())
		if _, err := lh.migrate("vm"); !failure.Is(err, failure.Conflict) || !strings.Contains(err.Error(), "vol-delete --pool werft "+name) {
			t.Errorf("migrate with an old %s = %v", name, err)
		}
		assertNoChange(t, lh.f, since)
	}
}

func TestMigrateFullKeepsClean(t *testing.T) {
	lh := newLegacyHost(t, "vm-full", "clean")
	m, err := lh.migrate("vm-full")
	if err != nil {
		t.Fatal(err)
	}
	if m.Keep != "clean" || m.Export != nil || m.Delete != nil {
		t.Errorf("migration = %+v", m)
	}
	if names, _ := lh.f.Snapshots("vm-full"); !slices.Equal(names, []string{"clean"}) {
		t.Errorf("snapshots = %q", names)
	}
	if got := lh.volumes("werft"); len(got) != 0 {
		t.Errorf("werft pool = %q, want nothing exported", got)
	}
}

// changes are the calls that change something. A refusal makes none of
// them; RefreshPool only makes libvirt read the directory again.
var changes = []string{"RedefineSnapshot", "RevertSnapshot", "DeleteSnapshot", "CreateSnapshot", "CreateVolume", "CreateVolumeFrom", "DeleteVolume", "DefineDomain", "UploadVolume"}

func assertNoChange(t *testing.T, f *virttest.Fake, since int) {
	t.Helper()
	for _, call := range f.Calls()[since:] {
		if slices.Contains(changes, strings.Fields(call)[0]) {
			t.Errorf("a refusal called %q", call)
		}
	}
}

func TestMigrateRefusesBeforeAnyChange(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		setup  func(lh *legacyHost)
		want   failure.Kind
	}{
		{"unknown domain", "none", func(*legacyHost) {}, failure.NotFound},
		{"running", "vm", func(lh *legacyHost) { lh.must(lh.f.StartDomain("vm")) }, failure.Conflict},
		{"old libvirt", "vm", func(lh *legacyHost) { lh.f.Version = virt.Version{Major: 11, Minor: 3} }, failure.Unsupported},
		{"BIOS", "bios", func(lh *legacyHost) {
			_, err := lh.f.DefineDomain(`<domain type='kvm'><name>bios</name><devices><disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm.qcow2'/><target dev='vda'/></disk></devices></domain>`)
			lh.must(err)
		}, failure.Unsupported},
		{"raw disk", "raw", func(lh *legacyHost) {
			lh.redefine("raw", "type='qcow2'/><source file='/img/", "type='raw'/><source file='/img/")
		}, failure.Unsupported},
		{"no loader path", "auto", func(lh *legacyHost) {
			lh.redefine("auto", "<loader readonly='yes' secure='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.ms.fd</loader>", "")
		}, failure.Unsupported},
		{"NVRAM in no pool", "vm", func(lh *legacyHost) { lh.must(lh.f.DeleteVolume("nvram", "vm_VARS.fd")) }, failure.Unsupported},
		{"disk in no pool", "vm", func(lh *legacyHost) { lh.must(lh.f.DeleteVolume("default", "vm.qcow2")) }, failure.Unsupported},
		{"native reset points only", "native", func(lh *legacyHost) {
			lh.redefine("native", "format='raw'>", "format='qcow2'>")
			lh.must(lh.f.CreateSnapshot("native", "provision-base", ""))
		}, failure.Conflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lh := newLegacyHost(t, "vm", "clean-install", "provision-base")
			tt.setup(lh)
			since := len(lh.f.Calls())
			m, err := lh.migrate(tt.domain)
			if !failure.Is(err, tt.want) || m != nil {
				t.Errorf("migrate = %+v, %v; want kind %s", m, err, tt.want)
			}
			assertNoChange(t, lh.f, since)
		})
	}
}

// redefine defines a second domain name from vm with old replaced by new.
func (lh *legacyHost) redefine(name, old, new string) {
	lh.t.Helper()
	text := strings.ReplaceAll(legacyVM("vm"), "<name>vm</name>", "<name>"+name+"</name>")
	if !strings.Contains(text, old) {
		lh.t.Fatalf("%q is not in the domain XML", old)
	}
	_, err := lh.f.DefineDomain(strings.Replace(text, old, new, 1))
	lh.must(err)
}

// TestMigrateFinishesAfterAFailure stops a migration at each step that
// changes something, and checks that the next run ends where an
// uninterrupted one does.
func TestMigrateFinishesAfterAFailure(t *testing.T) {
	steps := []string{
		"RedefineSnapshot vm provision-base",
		"RevertSnapshot vm provision-base",
		"RedefineSnapshot vm clean-install",
		"RevertSnapshot vm clean-install",
		"CreateVolumeFrom werft vm-clean-install.qcow2",
		"CreateVolumeFrom werft vm-clean-install_VARS.qcow2",
		"DeleteVolume nvram vm_VARS.fd.clean-install.bak",
		"DeleteSnapshot vm clean-install",
		"DeleteVolume nvram vm_VARS.fd.full.bak",
		"DeleteSnapshot vm full",
		"CreateVolumeFrom nvram vm_VARS.qcow2",
		"DefineDomain vm",
		"DeleteVolume nvram vm_VARS.fd",
		"DeleteVolume nvram vm_VARS.fd.provision-base.bak",
		"DeleteSnapshot vm provision-base",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			lh := newLegacyHost(t, "vm", "clean-install", "full", "provision-base")
			boom := errors.New("boom")
			lh.f.FailOn(step, boom)
			if _, err := lh.migrate("vm"); !errors.Is(err, boom) {
				t.Fatalf("migrate with %s failing = %v", step, err)
			}
			lh.f.FailOn(step, nil)
			if _, err := lh.migrate("vm"); err != nil {
				t.Fatalf("second migrate: %v", err)
			}
			lh.assertMigrated()
		})
	}
}

func TestMigrateKeepsRawNVRAMWhenTheRedefineFails(t *testing.T) {
	lh := newLegacyHost(t, "vm", "provision-base")
	lh.f.FailOn("DefineDomain vm", errors.New("boom"))
	if _, err := lh.migrate("vm"); err == nil || !strings.Contains(err.Error(), "keeps its raw NVRAM") {
		t.Fatalf("migrate = %v", err)
	}
	if got := lh.volumes("nvram"); slices.Contains(got, "vm_VARS.qcow2") {
		t.Errorf("nvram pool = %q; the qcow2 NVRAM of the failed redefine stays", got)
	}
	if err := resetpoint.Guard(lh.f, "vm"); !failure.Is(err, failure.Unsupported) {
		t.Errorf("guard = %v; the domain must keep its raw NVRAM", err)
	}
}

func TestMigrateNamesTheSnapshotCommandWhenTheLastStepFails(t *testing.T) {
	lh := newLegacyHost(t, "vm", "provision-base")
	lh.f.FailOn("CreateSnapshot vm provision-base", errors.New("boom"))
	if _, err := lh.migrate("vm"); err == nil || !strings.Contains(err.Error(), "werft snapshot vm provision-base") {
		t.Fatalf("migrate = %v", err)
	}
	if err := resetpoint.Guard(lh.f, "vm"); err != nil {
		t.Errorf("guard = %v; the domain is migrated", err)
	}
}

// unlistedNVRAM is a host where the raw NVRAM of vm lies in virt.NVRAMDir
// and a pool lists it only after a refresh.
type unlistedNVRAM struct{ *virttest.Fake }

func (h unlistedNVRAM) RefreshPool(name string) error {
	if err := h.Fake.RefreshPool(name); err != nil {
		return err
	}
	if _, err := h.VolumePath(name, "werft-unit-vm_VARS.fd"); err == nil {
		return nil
	}
	_, err := h.CreateVolume(name, "<volume><name>werft-unit-vm_VARS.fd</name><capacity>1</capacity></volume>")
	return err
}

// TestMigrateMakesTheNVRAMPoolOnlyInRun plans a migration of a domain whose
// NVRAM no pool lists: the plan changes nothing, and Run makes the pool.
func TestMigrateMakesTheNVRAMPoolOnlyInRun(t *testing.T) {
	h := unlistedNVRAM{virttest.New()}
	for pool, path := range map[string]string{"default": "/img", "werft": "/werft"} {
		if err := virt.EnsurePool(h, pool, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.CreateVolume("default", "<volume><name>werft-unit-vm.qcow2</name><capacity>1</capacity></volume>"); err != nil {
		t.Fatal(err)
	}
	xml := strings.Replace(legacyVM("werft-unit-vm"), "/nvram/", virt.NVRAMDir+"/", 1)
	if _, err := h.DefineDomain(xml); err != nil {
		t.Fatal(err)
	}
	since := len(h.Calls())
	m, err := resetpoint.PlanMigration(h, "werft-unit-vm", "werft")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range h.Calls()[since:] {
		if strings.HasPrefix(call, "DefinePool") || strings.HasPrefix(call, "RefreshPool") {
			t.Errorf("the plan called %s", call)
		}
	}
	if err := m.Run(h, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if names, err := h.Volumes(virt.NVRAMPool); err != nil || !slices.Equal(names, []string{"werft-unit-vm_VARS.qcow2"}) {
		t.Errorf("nvram pool = %q, %v", names, err)
	}
}
