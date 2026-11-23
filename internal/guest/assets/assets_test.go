package assets_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/guest/assets"
)

func TestPinentryAnswersGETPIN(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "werft-passphrase"), []byte("pa%ss\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "pinentry-auto")
	if err := os.WriteFile(script, assets.PinentryAuto, 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", script)
	c.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir)
	c.Stdin = strings.NewReader("OPTION ttyname=x\nGETPIN\nBYE\n")
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "OK Pleased to meet you\nOK\nD pa%25ss\nOK\nOK closing connection\n"
	if string(out) != want {
		t.Errorf("pinentry said\n%s\nwant\n%s", out, want)
	}
}

const sealKey = "ssh-ed25519 AAAAseal seal"

// runSealTCP runs seal-tcp.sh with its fixed paths moved under a temp dir,
// and a systemctl stub that reports version.
func runSealTCP(t *testing.T, version, keys string) (dir string, out []byte, err error) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, filepath.Join(dir, "root/.ssh"), filepath.Join(dir, "dbus")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\necho 'systemd " + version + " (" + version + ".4-1)'\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dbus/machine-id"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if keys != "" {
		if err := os.WriteFile(filepath.Join(dir, "root/.ssh/authorized_keys"), []byte(keys), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.NewReplacer(
		"/root/.ssh/authorized_keys", filepath.Join(dir, "root/.ssh/authorized_keys"),
		"/var/lib/dbus/machine-id", filepath.Join(dir, "dbus/machine-id"),
	).Replace(assets.SealTCP)
	c := exec.Command("sh", "-c", script, "sh", "tester", "", sealKey)
	c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err = c.Output()
	return dir, out, err
}

func TestSealTCPRemovesOnlyTheRunKey(t *testing.T) {
	const other = "ssh-ed25519 AAAAother user"
	tests := map[string]struct{ keys, want string }{
		"key among others":         {other + "\n\n" + sealKey + "\n", other + "\n\n"},
		"key alone":                {"\n" + sealKey + "\n", "\n"},
		"longer line stays":        {sealKey + " more\n" + sealKey + "\n", sealKey + " more\n"},
		"key twice, after reboots": {sealKey + "\n" + other + "\n" + sealKey + "\n", other + "\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir, out, err := runSealTCP(t, "255", tt.keys)
			if err != nil {
				t.Fatalf("seal-tcp.sh: %v", err)
			}
			if len(out) != 0 {
				t.Errorf("stdout = %q, want nothing: seal reads the guest facts there", out)
			}
			got, err := os.ReadFile(filepath.Join(dir, "root/.ssh/authorized_keys"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("authorized_keys = %q, want %q", got, tt.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "root/.ssh/authorized_keys.werft")); !os.IsNotExist(err) {
				t.Errorf("the temporary file is left: %v", err)
			}
			link, err := os.Readlink(filepath.Join(dir, "dbus/machine-id"))
			if err != nil || link != "/etc/machine-id" {
				t.Errorf("D-Bus machine-id link = %q, %v", link, err)
			}
		})
	}
}

func TestSealTCPFails(t *testing.T) {
	tests := map[string]struct{ version, keys string }{
		"grep error: no key file": {"255", ""},
		"another systemd":         {"256", sealKey + "\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir, _, err := runSealTCP(t, tt.version, tt.keys)
			if err == nil {
				t.Fatal("seal-tcp.sh passed")
			}
			if got, err := os.ReadFile(filepath.Join(dir, "dbus/machine-id")); err != nil || string(got) != "old\n" {
				t.Errorf("D-Bus machine-id = %q, %v; a failed run must leave it", got, err)
			}
		})
	}
}
