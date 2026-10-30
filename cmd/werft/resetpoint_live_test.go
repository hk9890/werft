//go:build live

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	"libvirt.org/go/libvirtxml"

	"github.com/hk9890/werft/internal/domxml"
	"github.com/hk9890/werft/internal/livetest"
	"github.com/hk9890/werft/internal/virt"
)

const (
	// NV | BS | RT, then the value "werft".
	efiVarValue = `\x07\x00\x00\x00werft`
	efiVarPath  = "/sys/firmware/efi/efivars/WerftResetTest-3f1b2c4d-5e6f-4a1b-9c8d-7e6f5a4b3c2d"
	marker      = "/root/werft-marker"
	ovmfVars    = "/usr/share/OVMF/OVMF_VARS_4M.fd"
)

// mark writes text to the marker file, and sets the UEFI variable when
// withVar is set, else removes it.
func mark(g *livetest.Guest, text string, withVar bool) {
	g.T.Helper()
	nv := "rm -f " + efiVarPath
	if withVar {
		nv = fmt.Sprintf("printf '%s' > %s", efiVarValue, efiVarPath)
	}
	g.Run(fmt.Sprintf("chattr -i %s 2>/dev/null; %s; echo %s > %s && sync", efiVarPath, nv, text, marker))
}

// guestState is the marker text and whether the UEFI variable is set.
func guestState(g *livetest.Guest) string {
	g.T.Helper()
	return strings.TrimSpace(g.Run(fmt.Sprintf("cat %s; test -e %s && echo var || echo novar", marker, efiVarPath)))
}

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	config := filepath.Join(t.TempDir(), "werft", "config.toml")
	livetest.Must(t, os.MkdirAll(filepath.Dir(config), 0o700), "create config directory")
	livetest.Must(t, os.WriteFile(config, []byte(fmt.Sprintf("pool = %q\n", livetest.Pool)), 0o600), "write config")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(filepath.Dir(config)))
}

// TestResetPointsLive takes a reset point with werft snapshot, changes disk
// and NVRAM, and checks that werft reset brings both back.
func TestResetPointsLive(t *testing.T) {
	isolate(t)
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-rp-cmd")

	g.Boot()
	mark(g, "one", false)
	g.Poweroff()
	if doc := werftJSON(t, 0, "snapshot", g.Name, "base"); doc["reset_point"] != "base" || doc["replaced"] != false {
		t.Errorf("snapshot = %v", doc)
	}
	werftJSON(t, 4, "snapshot", g.Name, "base")

	g.Boot()
	mark(g, "two", true)
	g.Poweroff()
	// Control: without a reset both changes survive a boot.
	g.Boot()
	if got := guestState(g); got != "two\nvar" {
		t.Fatalf("state before the reset = %q", got)
	}
	werftJSON(t, 4, "reset", "--yes", g.Name, "base")
	g.Poweroff()

	if doc := werftJSON(t, 0, "snapshots", g.Name); !reflect.DeepEqual(doc["reset_points"], []any{"base"}) {
		t.Errorf("snapshots = %v", doc)
	}
	werftJSON(t, 5, "reset", g.Name, "base")
	werftJSON(t, 0, "reset", "--yes", g.Name, "base")
	g.Boot()
	if got := guestState(g); got != "one\nnovar" {
		t.Errorf("state after the reset = %q, want the marker and NVRAM of the reset point", got)
	}
	g.Poweroff()
}

// TestGuardRefusesRawNVRAMLive gives a scratch domain a raw NVRAM, which a
// libvirt snapshot leaves out without an error, and checks that werft
// refuses it.
func TestGuardRefusesRawNVRAMLive(t *testing.T) {
	isolate(t)
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-rp-raw")
	toRawNVRAM(t, h, g)

	doc := werftJSON(t, 9, "snapshot", g.Name)
	if msg := fmt.Sprint(doc["error"]); !strings.Contains(msg, "werft migrate "+g.Name) {
		t.Errorf("refusal = %v, want it to name werft migrate", doc)
	}
	werftJSON(t, 9, "reset", "--yes", g.Name)
	if names, err := h.Snapshots(g.Name); err != nil || len(names) != 0 {
		t.Fatalf("snapshots after the refusal = %q, %v", names, err)
	}

	// Control: libvirt itself takes the snapshot, and it holds no NVRAM.
	livetest.Must(t, h.CreateSnapshot(g.Name, "trap", ""), "snapshot of a raw-NVRAM domain")
	text, err := h.SnapshotXML(g.Name, "trap")
	livetest.Must(t, err, "snapshot XML")
	var s libvirtxml.DomainSnapshot
	livetest.Must(t, s.Unmarshal(text), "parse snapshot XML")
	if nv, _ := domxml.NVRAMOf(s.Domain); nv.Format != "raw" {
		t.Errorf("the control snapshot has NVRAM format %q, want raw", nv.Format)
	}
}

// toRawNVRAM gives the guest a raw NVRAM volume <name>_VARS.fd and firmware
// autoselection, as vm's build VMs have, and removes the volume when the
// test ends.
func toRawNVRAM(t *testing.T, h virt.Hypervisor, g *livetest.Guest) string {
	t.Helper()
	raw := g.Name + "_VARS.fd"
	livetest.DeleteVolumes(h, raw)
	t.Cleanup(func() { livetest.DeleteVolumes(h, raw) })
	livetest.Upload(t, h, raw, ovmfVars)
	text, err := h.DomainXML(g.Name, true)
	livetest.Must(t, err, "domain XML")
	d, err := domxml.Parse(text)
	livetest.Must(t, err, "parse domain XML")
	d.OS.NVRam = &libvirtxml.DomainNVRam{NVRam: livetest.PoolPath + "/" + raw, Format: "raw"}
	d.OS.Firmware = "efi"
	text, err = d.Marshal()
	livetest.Must(t, err, "domain XML")
	_, err = h.DefineDomain(text)
	livetest.Must(t, err, "redefine with raw NVRAM")
	if text, err = h.DomainXML(g.Name, true); err != nil || !strings.Contains(text, "firmware='efi'") {
		t.Fatalf("the domain lost firmware autoselection: %v\n%s", err, text)
	}
	return raw
}

// TestMigrateLive puts a scratch domain into the layout vm leaves, without
// root, and migrates it with werft migrate. A reset point of vm is a
// qemu-img snapshot of the disk plus a copy of the raw NVRAM: libvirt takes
// the qemu-img snapshot when the NVRAM is raw, and forgets it when only its
// metadata is deleted.
func TestMigrateLive(t *testing.T) {
	isolate(t)
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-rp-mig")
	raw := toRawNVRAM(t, h, g)
	exports := []string{g.Name + "-clean-install.qcow2", g.Name + "-clean-install_VARS.qcow2"}
	leftovers := append([]string{g.Name + "_VARS.qcow2"}, exports...)
	for _, label := range []string{"clean-install", "provision-base", "extra"} {
		leftovers = append(leftovers, raw+"."+label+".bak")
	}
	livetest.DeleteVolumes(h, leftovers...)
	t.Cleanup(func() { livetest.DeleteVolumes(h, leftovers...) })
	l := golibvirtConn(t)

	vmPoint := func(label string) {
		t.Helper()
		livetest.Must(t, h.CreateSnapshot(g.Name, label, ""), "qemu-img snapshot "+label)
		dom, err := l.DomainLookupByName(g.Name)
		livetest.Must(t, err, "look up "+g.Name)
		s, err := l.DomainSnapshotLookupByName(dom, label, 0)
		livetest.Must(t, err, "look up snapshot "+label)
		livetest.Must(t, l.DomainSnapshotDelete(s, golibvirt.DomainSnapshotDeleteMetadataOnly), "forget snapshot "+label)
		_, err = h.CreateVolumeFrom(livetest.Pool, fmt.Sprintf("<volume><name>%s.%s.bak</name><capacity>0</capacity><target><format type='raw'/></target></volume>", raw, label), livetest.Pool, raw)
		livetest.Must(t, err, "copy NVRAM for "+label)
	}
	for i, label := range []string{"clean-install", "provision-base", "extra"} {
		g.Boot()
		mark(g, label, i == 2)
		g.Poweroff()
		vmPoint(label)
	}
	g.Boot()
	mark(g, "now", true)
	g.Poweroff()
	if names, err := h.Snapshots(g.Name); err != nil || len(names) != 0 {
		t.Fatalf("the layout of vm has libvirt snapshots %q, %v", names, err)
	}

	werftJSON(t, 5, "migrate", g.Name)
	doc := werftJSON(t, 0, "migrate", "--yes", g.Name)
	want := map[string]any{"domain": g.Name, "kept": "provision-base", "export_pool": livetest.Pool,
		"exported": []any{exports[0], exports[1]}, "deleted": []any{"extra"}}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("migrate = %v, want %v", doc, want)
	}

	if names, err := h.Snapshots(g.Name); err != nil || !slices.Equal(names, []string{"provision-base"}) {
		t.Errorf("snapshots after migrate = %q, %v", names, err)
	}
	text, err := h.DomainXML(g.Name, true)
	livetest.Must(t, err, "domain XML")
	d, err := domxml.Parse(text)
	livetest.Must(t, err, "parse domain XML")
	nv, _ := domxml.NVRAMOf(d)
	if l, _ := domxml.LoaderOf(d); nv.Format != "qcow2" || nv.Path != livetest.PoolPath+"/"+g.Name+"_VARS.qcow2" || l.Path == "" {
		t.Errorf("after migrate NVRAM = %+v, loader = %+v", nv, l)
	}
	livetest.Must(t, h.RefreshPool(livetest.Pool), "refresh pool")
	names, err := h.Volumes(livetest.Pool)
	livetest.Must(t, err, "list pool")
	for _, gone := range []string{raw, raw + ".clean-install.bak", raw + ".provision-base.bak", raw + ".extra.bak"} {
		if slices.Contains(names, gone) {
			t.Errorf("%s is left after migrate", gone)
		}
	}
	for _, e := range exports {
		if !slices.Contains(names, e) {
			t.Errorf("export %s is missing", e)
		}
	}

	g.Boot()
	if got := guestState(g); got != "provision-base\nnovar" {
		t.Errorf("state after migrate = %q, want disk and NVRAM of provision-base", got)
	}
	mark(g, "later", true)
	g.Poweroff()
	werftJSON(t, 0, "reset", "--yes", g.Name, "provision-base")
	g.Boot()
	if got := guestState(g); got != "provision-base\nnovar" {
		t.Errorf("state after a reset to the migrated point = %q", got)
	}
	g.Poweroff()
	werftJSON(t, 4, "migrate", "--yes", g.Name)
}

// golibvirtConn is a raw go-libvirt connection, for the metadata-only
// snapshot delete that builds the layout of vm; werft itself never needs it.
func golibvirtConn(t *testing.T) *golibvirt.Libvirt {
	t.Helper()
	socket := "/var/run/libvirt/libvirt-sock"
	if _, err := os.Stat(socket); err != nil {
		socket = "/var/run/libvirt/virtqemud-sock"
	}
	l := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(socket)))
	livetest.Must(t, l.ConnectToURI(golibvirt.QEMUSystem), "connect to libvirt")
	t.Cleanup(func() { _ = l.Disconnect() })
	return l
}
