package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/virt"
)

const imagesTOML = `[[image]]
name = "ubuntu-26-04-plain-20261102"
image = "ubuntu-26-04"
tier = "plain"
version = "20261102"
date = 2026-11-02T21:00:00Z
source = "ubuntu-26-04"
label = "provision-base"
pool = "werft"
disk = "ubuntu-26-04-plain-20261102.qcow2"
nvram = "ubuntu-26-04-plain-20261102_VARS.qcow2"
user = "hans"
family = "ubuntu"
systemd = 259
`

func TestImages(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	lh := newLifecycleHost(t)

	if r := lh.run("--json", "images"); r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("images without images.toml = %+v", r)
	}
	if err := os.MkdirAll(filepath.Join(state, "werft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "werft", "images.toml"), []byte(imagesTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	r := lh.run("--json", "images")
	var list []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || r.code != 0 || len(list) != 1 {
		t.Fatalf("images --json = %+v, %v", r, err)
	}
	if e := list[0]; e["image"] != "ubuntu-26-04" || e["tier"] != "plain" || e["version"] != "20261102" ||
		e["date"] != "2026-11-02T21:00:00Z" || e["user"] != "hans" || e["systemd"] != float64(259) {
		t.Errorf("images entry = %v", e)
	}

	r = lh.run("images")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "IMAGE TIER VERSION DATE USER" ||
		!strings.HasPrefix(strings.Join(strings.Fields(lines[1]), " "), "ubuntu-26-04 plain 20261102 2026-11-0") {
		t.Errorf("images = %+v", r)
	}
}

func TestSealNeedsConfirmation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	if r := lh.run("seal", "vm1"); r.code != 2 {
		t.Errorf("seal with one argument = %+v, want usage", r)
	}
	if r := lh.run("seal", "vm1", "clean"); r.code != 2 {
		t.Errorf("seal of another label = %+v, want usage", r)
	}
	qcow2 := strings.Replace(lifecycleVM(lh.nvramDir), "<nvram format='raw'>", "<nvram format='qcow2'>", 1)
	if err := lh.fake.UndefineDomain("vm1", virt.UndefineKeepNVRAM); err != nil {
		t.Fatal(err)
	}
	if _, err := lh.fake.DefineDomain(qcow2); err != nil {
		t.Fatal(err)
	}
	if err := virt.EnsurePool(lh.fake, "vars", lh.nvramDir); err != nil {
		t.Fatal(err)
	}
	if _, err := lh.fake.CreateVolume("vars", "<volume><name>vm1_VARS.fd</name><capacity>0</capacity></volume>"); err != nil {
		t.Fatal(err)
	}
	if r := lh.run("--json", "seal", "vm1", "provision-base"); r.code != 3 {
		t.Errorf("seal without the reset point = %+v, want not_found", r)
	}
	if err := lh.fake.CreateSnapshot("vm1", "provision-base", ""); err != nil {
		t.Fatal(err)
	}
	if r := lh.run("--json", "seal", "vm1", "provision-base"); r.code != 5 || !strings.Contains(r.stdout, "--yes") {
		t.Errorf("seal without --yes = %+v, want needs_confirmation", r)
	}
	if calls := strings.Join(lh.fake.Calls(), "\n"); strings.Contains(calls, "RevertSnapshot") || strings.Contains(calls, "CreateVolumeFrom") {
		t.Errorf("seal without --yes changed something:\n%s", calls)
	}
}
