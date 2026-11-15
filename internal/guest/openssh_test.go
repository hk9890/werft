package guest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/guest"
)

// fakeSSH puts an ssh on PATH that writes its arguments and stdin to stdout
// and exits with $SSH_EXIT, after $SSH_SLEEP seconds.
func fakeSSH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$*\"\ncat\nsleep \"${SSH_SLEEP:-0}\"\nexit \"${SSH_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SSH_EXIT", "")
	t.Setenv("SSH_SLEEP", "")
}

func TestOpenSSHRun(t *testing.T) {
	fakeSSH(t)
	o := guest.OpenSSH{Config: "/state/ssh_config"}
	addr := guest.Addr{User: "u", Host: "vsock/7"}
	var out bytes.Buffer
	if err := o.Run(context.Background(), addr, "cat >/dev/null", guest.Line("s3cr%t"), &out, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "-F /state/ssh_config u@vsock/7 -- cat >/dev/null\ns3cr%t\n" {
		t.Errorf("ssh got %q", out.String())
	}

	out.Reset()
	if err := o.Run(context.Background(), addr, "true", nil, &out, &out); err != nil || out.String() != "-F /state/ssh_config u@vsock/7 -- true\n" {
		t.Errorf("a nil stdin: %q, %v", out.String(), err)
	}
}

func TestOpenSSHRunTellsTheCommandFromTheConnection(t *testing.T) {
	fakeSSH(t)
	o := guest.OpenSSH{Config: "/state/ssh_config"}
	addr := guest.Addr{User: "u", Host: "192.0.2.1"}

	t.Setenv("SSH_EXIT", "3")
	var exit *guest.ExitError
	if err := o.Run(context.Background(), addr, "false", nil, &bytes.Buffer{}, &bytes.Buffer{}); !errors.As(err, &exit) || exit.Code != 3 {
		t.Errorf("a failed command: %v", err)
	}

	t.Setenv("SSH_EXIT", "255")
	err := o.Run(context.Background(), addr, "true", nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || errors.As(err, &exit) || !strings.Contains(err.Error(), "ssh u@192.0.2.1") {
		t.Errorf("a failed connection: %v", err)
	}
}

func TestOpenSSHRunStopsWithItsContext(t *testing.T) {
	fakeSSH(t)
	t.Setenv("SSH_SLEEP", "30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := guest.OpenSSH{Config: "/c"}.Run(ctx, guest.Addr{User: "u", Host: "h"}, "true", nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a cancelled ssh succeeded")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("ssh ran %v after its context ended", d)
	}
}

func TestLoginPause(t *testing.T) {
	var got []time.Duration
	for attempt := 1; attempt <= 6; attempt++ {
		got = append(got, guest.LoginPause(attempt))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pauses = %v, want %v", got, want)
			break
		}
	}
}

// TestWriteHostConfig asks ssh how it resolves the lease name with the
// config.
func TestWriteHostConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_config")
	if err := guest.WriteHostConfig(path, "abc123", guest.Addr{User: "tester", Host: "vsock/7"}, "/leases/abc123/id_ed25519"); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ssh", "-G", "-F", path, "abc123").Output()
	if err != nil {
		t.Skipf("no ssh -G: %v", err)
	}
	for _, line := range []string{"hostname vsock/7", "user tester", "identityfile /leases/abc123/id_ed25519", "identitiesonly yes",
		"proxycommand " + guest.SSHProxy + " %h %p", "proxyusefdpass yes"} {
		if !strings.Contains(string(out), line+"\n") {
			t.Errorf("ssh -G lacks %q:\n%s", line, out)
		}
	}
}
