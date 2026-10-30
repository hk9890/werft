package guest_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/guest"
)

func TestQuoteSurvivesTheShell(t *testing.T) {
	for _, s := range []string{"plain", "it's", `a "b" $c \d`, "", "x'y'z"} {
		out, err := exec.Command("sh", "-c", "printf %s "+guest.Quote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("Quote(%q) gives %q, %v", s, out, err)
		}
	}
}

func TestRootCommandKeepsArgumentsApart(t *testing.T) {
	cmd := guest.RootCommand(`printf '%s|' "$1" "$2"`, "a b", "it's")
	if !strings.HasPrefix(cmd, "sudo -k -S -p '' sh -c ") {
		t.Fatalf("cmd = %q", cmd)
	}
	out, err := exec.Command("sh", "-c", strings.TrimPrefix(cmd, "sudo -k -S -p '' ")).Output()
	if err != nil || string(out) != "a b|it's|" {
		t.Errorf("ran as %q, %v", out, err)
	}
}

func TestWriteTar(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "checks", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checks", "run.sh"), []byte("run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checks", "sub", "a.sh"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := guest.WriteTar(&buf, []guest.File{
		{Name: "helper", Data: []byte("h"), Mode: 0o755},
		{Name: "system", Source: filepath.Join(dir, "checks")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		got[h.Name] = string(data) + " " + os.FileMode(h.Mode).String()
	}
	want := map[string]string{
		"helper":          "h -rwxr-xr-x",
		"system/":         " -rwxr-xr-x",
		"system/run.sh":   "run -rwxr-xr-x",
		"system/sub/":     " -rwxr-xr-x",
		"system/sub/a.sh": "a -rw-r--r--",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %q, want %q", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("entries = %q", got)
	}
}

func TestWriteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "ssh_config")
	if err := guest.WriteConfig(path, []string{"/state/id_ed25519"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"Host vsock/*\n  ProxyCommand /usr/lib/systemd/systemd-ssh-proxy %h %p", "IdentityFile /state/id_ed25519", "BatchMode yes", "UserKnownHostsFile /dev/null"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config lacks %q:\n%s", want, data)
		}
	}
	if cmd := (guest.OpenSSH{Config: path}).Command(guest.Addr{User: "u", Host: "vsock/3"}); cmd != "ssh -F "+path+" u@vsock/3" {
		t.Errorf("Command = %q", cmd)
	}
}
