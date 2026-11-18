package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The log of a failed provision ends with the failure, as stderr does, so
// the log alone tells what happened.
func TestProvisionLogEndsWithTheFailure(t *testing.T) {
	rh := newRPHost(t)
	if err := rh.fake.StartDomain("vm"); err != nil {
		t.Fatal(err)
	}
	recipe := filepath.Join(t.TempDir(), "recipe.toml")
	if err := os.WriteFile(recipe, []byte("[[steps]]\ntype = \"snapshot\"\nlabel = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := rh.run("", false, "provision", "--yes", "--recipe", recipe, "vm")
	if r.code != 4 || !strings.Contains(r.stderr, "left running for inspection") {
		t.Fatalf("provision = %+v", r)
	}
	logs, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "werft", "logs", "provision-vm-*.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %q, %v", logs, err)
	}
	data, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	tail := strings.Join(lines[max(0, len(lines)-4):], "\n")
	if !strings.Contains(tail, "werft: ") || !strings.Contains(tail, "left running for inspection") || !strings.HasPrefix(lines[len(lines)-1], "log: ") {
		t.Errorf("the log ends with\n%s", tail)
	}
}

func TestPrepareRecipe(t *testing.T) {
	rh := newRPHost(t)
	saved := guestEnsureKey
	guestEnsureKey = func(string) error { return nil }
	t.Cleanup(func() { guestEnsureKey = saved })

	r := rh.run("", false, "prepare", "vm")
	if r.code != 10 || !strings.Contains(r.stderr, "No recipe for vm, so no keyring item or pass entry for the guest password") {
		t.Errorf("prepare without a recipe = %+v", r)
	}
	r = rh.run("", false, "prepare", "--recipe", "/none.toml", "vm")
	if r.code != 3 || !strings.Contains(r.stderr, "/none.toml does not exist") {
		t.Errorf("prepare --recipe = %+v", r)
	}
}

func TestProvisionKeyNeedsAUSBPassthroughStep(t *testing.T) {
	recipe := filepath.Join(t.TempDir(), "recipe.toml")
	if err := os.WriteFile(recipe, []byte("[[steps]]\ntype = \"snapshot\"\nlabel = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI("", false, "provision", "--key", "/media/tester/BACKUP", "--recipe", recipe, "vm")
	if r.code != 2 || !strings.Contains(r.stderr, "has no usb-passthrough step") {
		t.Errorf("provision --key = %+v", r)
	}
}
