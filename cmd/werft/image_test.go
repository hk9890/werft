package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/config"
	"github.com/hk9890/werft/internal/image"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/virt"
)

// TestDiskPasswordSpecReadsTheRecipeOfTheImage: both tiers read the
// recipe config.toml gives the image, so the record of a sealed LUKS image
// names its pass entry, and an agent lease can read the password.
func TestDiskPasswordSpecReadsTheRecipeOfTheImage(t *testing.T) {
	dir := t.TempDir()
	withEntry := filepath.Join(dir, "omarchy.toml")
	if err := os.WriteFile(withEntry, []byte("[guest_password]\npass = \"x/y\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "global.toml")
	if err := os.WriteFile(global, []byte("[guest_password]\npass = \"global\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Recipe: global, Recipes: map[string]string{"omarchy-4-0": withEntry}}
	for _, s := range []*image.Seal{
		{BuildVM: "omarchy-4-0", Image: "omarchy-4-0", Tier: "plain"},
		{BuildVM: "omarchy-4-0-full", Image: "omarchy-4-0", Tier: "full"},
	} {
		spec, err := diskPasswordSpec(cfg, "", s.Image, s.BuildVM)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Pass != "x/y" || !strings.Contains(spec.Prompt, s.BuildVM) {
			t.Errorf("spec of %s = %+v, want pass x/y and a prompt that names it", s.BuildVM, spec)
		}
	}
	spec, err := diskPasswordSpec(config.Config{}, "", "vm", "vm")
	if err != nil || spec.Pass != "" || spec.Prompt == "" {
		t.Errorf("spec without a recipe = %+v, %v, want a prompt only", spec, err)
	}
	spec, err = diskPasswordSpec(cfg, global, "omarchy-4-0", "omarchy-4-0")
	if err != nil || spec.Pass != "global" {
		t.Errorf("spec with --recipe = %+v, %v, want pass global over the configured recipe", spec, err)
	}
}

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
user = "tester"
family = "ubuntu"
systemd = 259
`

const fullImageTOML = `
[[image]]
name = "ubuntu-26-04-full-20261106"
image = "ubuntu-26-04"
tier = "full"
version = "20261106"
date = 2026-11-06T21:00:00Z
source = "ubuntu-26-04-full"
label = "clean"
pool = "werft"
disk = "ubuntu-26-04-full-20261106.qcow2"
nvram = "ubuntu-26-04-full-20261106_VARS.qcow2"
user = "tester"
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
	if err := os.WriteFile(filepath.Join(state, "werft", "images.toml"), []byte(imagesTOML+fullImageTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	r := lh.run("--json", "images")
	var list []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || r.code != 0 || len(list) != 2 {
		t.Fatalf("images --json = %+v, %v", r, err)
	}
	if e := list[0]; e["image"] != "ubuntu-26-04" || e["tier"] != "plain" || e["version"] != "20261102" ||
		e["date"] != "2026-11-02T21:00:00Z" || e["user"] != "tester" || e["systemd"] != float64(259) {
		t.Errorf("images entry = %v", e)
	}
	if e := list[1]; e["image"] != "ubuntu-26-04" || e["tier"] != "full" || e["source"] != "ubuntu-26-04-full" || e["label"] != "clean" {
		t.Errorf("full images entry = %v", e)
	}

	r = lh.run("images")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "NAME IMAGE TIER VERSION DATE (UTC) USER" ||
		!strings.HasPrefix(strings.Join(strings.Fields(lines[1]), " "), "ubuntu-26-04-plain-20261102 ubuntu-26-04 plain 20261102 2026-11-02 21:00 tester") {
		t.Errorf("images = %+v", r)
	}
}

// TestImagesPrune: werft images --prune deletes an old version that no
// lease uses, asks first, and werft --json images no longer lists it. The
// version a lease names and the newest version stay.
func TestImagesPrune(t *testing.T) {
	lh := newPruneHost(t)
	addLease(t, lh, "abc123", time.Now())

	if r := lh.run("images", "--yes"); r.code != 2 {
		t.Errorf("images --yes = %+v, want usage", r)
	}
	if r := lh.run("--json", "images", "--prune"); r.code != 5 || !strings.Contains(r.stdout, "vm-plain-20261101") || !strings.Contains(r.stdout, "--yes") {
		t.Errorf("images --prune without --yes = %+v, want needs_confirmation naming the version", r)
	}
	if calls := strings.Join(lh.fake.Calls(), "\n"); strings.Contains(calls, "DeleteVolume") {
		t.Errorf("images --prune without --yes deleted a volume:\n%s", calls)
	}

	r := lh.run("--json", "images", "--prune", "-y")
	var doc struct {
		Removed []struct{ Name string } `json:"removed"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || r.code != 0 || len(doc.Removed) != 1 || doc.Removed[0].Name != "vm-plain-20261101" {
		t.Fatalf("images --prune --yes = %+v, %v", r, err)
	}
	r = lh.run("--json", "images")
	var list []struct{ Name string }
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list) != 2 || list[0].Name != "vm-plain-20261102" || list[1].Name != "vm-plain-20261103" {
		t.Errorf("images after the prune = %+v, %v", r, err)
	}
	if r := lh.run("images", "--prune"); r.code != 0 || !strings.Contains(r.stdout, "nothing to prune") {
		t.Errorf("a second images --prune = %+v, want nothing to prune", r)
	}
}

// TestImagesPruneRefusesANewerCatalogBeforeItAsks: without a terminal,
// prune of an images.toml with unknown keys fails with unsupported, not
// needs_confirmation, and deletes nothing.
func TestImagesPruneRefusesANewerCatalogBeforeItAsks(t *testing.T) {
	lh := newPruneHost(t)
	writeNewerCatalog(t, filepath.Join(os.Getenv("XDG_STATE_HOME"), "werft", "images.toml"))
	if r := lh.run("--json", "images", "--prune"); r.code != 9 || !strings.Contains(r.stdout, "image.newer_field") {
		t.Errorf("images --prune of an images.toml of a newer werft = %+v, want unsupported", r)
	}
	if calls := strings.Join(lh.fake.Calls(), "\n"); strings.Contains(calls, "DeleteVolume") {
		t.Errorf("images --prune deleted a volume:\n%s", calls)
	}
}

// writeNewerCatalog appends a key that this werft does not know to the
// last record of the images.toml at path, as a newer werft could write it.
func writeNewerCatalog(t *testing.T, path string) {
	t.Helper()
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(old) == 0 {
		old = []byte("[[image]]\nname = \"vm-plain-20261101\"\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(old, "newer_field = \"x\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newPruneHost is a host with three versions of image vm, tier plain, in
// pool werft, and their records in images.toml.
func newPruneHost(t *testing.T) *lifecycleHost {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	lh := newLifecycleHost(t)
	if err := virt.EnsurePool(lh.fake, "werft", "/werft"); err != nil {
		t.Fatal(err)
	}
	var catalog strings.Builder
	for _, name := range []string{"vm-plain-20261101", "vm-plain-20261102", "vm-plain-20261103"} {
		fmt.Fprintf(&catalog, "[[image]]\nname = %q\nimage = \"vm\"\ntier = \"plain\"\npool = \"werft\"\ndisk = \"%s.qcow2\"\nnvram = \"%s_VARS.qcow2\"\n\n", name, name, name)
		for _, v := range []string{name + ".qcow2", name + "_VARS.qcow2"} {
			if _, err := lh.fake.CreateVolume("werft", "<volume><name>"+v+"</name><capacity>0</capacity></volume>"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(state, "werft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "werft", "images.toml"), []byte(catalog.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return lh
}

// TestImagesPruneReapsFirst: werft images --prune runs the reap pass first,
// so it releases an expired lease and deletes the version that lease named
// in the same run.
func TestImagesPruneReapsFirst(t *testing.T) {
	lh := newPruneHost(t)
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	domain := addLease(t, lh, "abc123", t0)
	setClock(t, t0.Add(lease.Lifetime))

	r := lh.run("--json", "images", "--prune", "--yes")
	var doc struct {
		Removed []struct{ Name string } `json:"removed"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || r.code != 0 || len(doc.Removed) != 2 ||
		doc.Removed[0].Name != "vm-plain-20261101" || doc.Removed[1].Name != "vm-plain-20261102" {
		t.Fatalf("images --prune --yes after the expiry = %+v, %v; want both old versions removed", r, err)
	}
	if !strings.Contains(r.stderr, "Released "+domain) {
		t.Errorf("images --prune stderr = %q, want the release of %s", r.stderr, domain)
	}
	if _, err := lh.fake.Domain(domain); err == nil {
		t.Errorf("the expired lease domain %s is still there", domain)
	}
}

func TestSealNeedsConfirmation(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
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
	writeNewerCatalog(t, filepath.Join(state, "werft", "images.toml"))
	if r := lh.run("--json", "seal", "vm1", "provision-base"); r.code != 9 || !strings.Contains(r.stdout, "image.newer_field") {
		t.Errorf("seal into an images.toml of a newer werft = %+v, want unsupported before the question", r)
	}
	if calls := strings.Join(lh.fake.Calls(), "\n"); strings.Contains(calls, "RevertSnapshot") || strings.Contains(calls, "CreateVolumeFrom") {
		t.Errorf("seal without --yes changed something:\n%s", calls)
	}
}
