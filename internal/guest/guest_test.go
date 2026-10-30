package guest_test

import (
	"archive/tar"
	"bytes"
	"context"
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
	if err := guest.WriteConfig(path, "/state/id_ed25519", "/home/u/.ssh/id_ed25519"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{
		"Host vsock/*\n  ProxyCommand /usr/lib/systemd/systemd-ssh-proxy %h %p",
		"Host *\n  IdentityFile /state/id_ed25519\n  IdentitiesOnly yes\n", "BatchMode yes", "UserKnownHostsFile /dev/null",
		"Match !tagged werft-key-only\n  IdentityFile /home/u/.ssh/id_ed25519\n",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config lacks %q:\n%s", want, data)
		}
	}
	if cmd := (guest.OpenSSH{Config: path}).Command(guest.Addr{User: "u", Host: "vsock/3"}); cmd != "ssh -F "+path+" u@vsock/3" {
		t.Errorf("Command = %q", cmd)
	}
	if args := (guest.OpenSSH{Config: path, KeyOnly: true}).Args(guest.Addr{User: "u", Host: "h"}, "true"); strings.Join(args, " ") != "-F "+path+" -P werft-key-only u@h -- true" {
		t.Errorf("KeyOnly args = %q", args)
	}
}

// The werft key is offered first, and alone with KeyOnly; no agent key is.
func TestWriteConfigOffersTheKeysInOrder(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	path := filepath.Join(t.TempDir(), "ssh_config")
	if err := guest.WriteConfig(path, "/state/id_ed25519", "/home/u/.ssh/id_ed25519"); err != nil {
		t.Fatal(err)
	}
	for keyOnly, want := range map[bool]string{
		false: "identitiesonly yes\nidentityfile /state/id_ed25519\nidentityfile /home/u/.ssh/id_ed25519\n",
		true:  "identitiesonly yes\nidentityfile /state/id_ed25519\n",
	} {
		o := guest.OpenSSH{Config: path, KeyOnly: keyOnly}
		args := append([]string{"-G"}, o.Args(guest.Addr{User: "u", Host: "192.0.2.1"}, "")...)
		out, err := exec.Command("ssh", args...).Output()
		if err != nil {
			t.Fatalf("ssh -G: %v", err)
		}
		var got strings.Builder
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "identit") {
				got.WriteString(line + "\n")
			}
		}
		if got.String() != want {
			t.Errorf("KeyOnly %v: ssh resolves\n%swant\n%s", keyOnly, got.String(), want)
		}
	}
}

func TestScriptCommandKeepsStdinFree(t *testing.T) {
	cmd := guest.ScriptCommand(`read -r line; printf '%s|%s' "$1" "$line"`, "it's")
	if cmd != `sh -c 'read -r line; printf '\''%s|%s'\'' "$1" "$line"' sh 'it'\''s'` {
		t.Errorf("cmd = %s", cmd)
	}
	c := exec.Command("sh", "-c", cmd)
	c.Stdin = guest.Line("s3cr t")
	out, err := c.Output()
	if err != nil || string(out) != "it's|s3cr t" {
		t.Errorf("ran as %q, %v", out, err)
	}
}

// shellFunc is a guest.Shell that runs in a function.
type shellFunc func(cmd string, stdin io.Reader) error

func (f shellFunc) Run(_ context.Context, _ guest.Addr, cmd string, stdin io.Reader, _, _ io.Writer) error {
	return f(cmd, stdin)
}

// Copy replaces the directory with the files, in one login. The command
// runs here in a shell, with the directory under a temp dir.
func TestCopy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "guest dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	logins := 0
	sh := shellFunc(func(cmd string, stdin io.Reader) error {
		logins++
		c := exec.Command("sh", "-c", cmd)
		c.Stdin = stdin
		return c.Run()
	})
	err := guest.Copy(context.Background(), sh, guest.Addr{}, dir, []guest.File{{Name: "helper", Data: []byte("h"), Mode: 0o755}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale")); !os.IsNotExist(err) {
		t.Error("the old content stayed")
	}
	info, err := os.Stat(filepath.Join(dir, "helper"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("helper: %v, %v", info, err)
	}
	if logins != 1 {
		t.Errorf("%d logins, want 1", logins)
	}
}
