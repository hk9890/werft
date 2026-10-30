//go:build live

package guest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/guest"
	"github.com/hk9890/werft/internal/livetest"
)

// TestOpenSSHLive drives the SSH adapter against a real guest over vsock,
// through a werft ssh_config: a command, a script with a secret on stdin,
// and a copy.
func TestOpenSSHLive(t *testing.T) {
	h := livetest.Connect(t)
	g := livetest.NewGuest(t, h, "werft-test-gl-ssh")
	g.Boot()

	dir := t.TempDir()
	config := filepath.Join(dir, "ssh_config")
	livetest.Must(t, guest.WriteConfig(config, g.Key.Private), "write the ssh_config")
	sh := guest.OpenSSH{Config: config}
	addr := guest.Addr{User: "root", Host: fmt.Sprintf("vsock/%d", g.CID)}
	ctx := context.Background()
	run := func(cmd string, stdin io.Reader) (string, error) {
		var out bytes.Buffer
		err := sh.Run(ctx, addr, cmd, stdin, &out, &out)
		return out.String(), err
	}

	if out, err := run("cat /proc/sys/kernel/hostname", nil); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("command: %q, %v", out, err)
	}
	var exit *guest.ExitError
	if _, err := run("exit 3", nil); !errors.As(err, &exit) || exit.Code != 3 {
		t.Errorf("a failed command: %v", err)
	}

	// Control: a config with another key is refused, so the login above
	// used the key of the werft config and no key of an SSH agent.
	other := filepath.Join(dir, "other")
	livetest.Must(t, guest.EnsureKey(other), "make another key")
	otherConfig := filepath.Join(dir, "other_config")
	livetest.Must(t, guest.WriteConfig(otherConfig, other), "write the other ssh_config")
	var refusal bytes.Buffer
	err := guest.OpenSSH{Config: otherConfig}.Run(ctx, addr, "true", nil, &refusal, &refusal)
	if err == nil || errors.As(err, &exit) || !strings.Contains(refusal.String(), "Permission denied") {
		t.Errorf("a login with another key: %v, %q", err, refusal.String())
	}

	script := guest.ScriptCommand(`IFS= read -r secret && printf '%s' "$secret" >"$1"`, "/root/secret")
	if strings.Contains(script, "pa ss") {
		t.Fatal("the secret is on the command line")
	}
	if out, err := run(script, strings.NewReader("  pa ss  \n")); err != nil {
		t.Fatalf("script with stdin: %q, %v", out, err)
	}
	if got := g.Run("cat /root/secret"); got != "  pa ss  " {
		t.Errorf("the guest got the secret %q", got)
	}

	checks := filepath.Join(dir, "checks")
	livetest.Must(t, os.MkdirAll(filepath.Join(checks, "sub"), 0o755), "make the checks dir")
	livetest.Must(t, os.WriteFile(filepath.Join(checks, "sub", "run.sh"), []byte("echo PASS\n"), 0o755), "write run.sh")
	// Control: the copy replaces the directory, so the stale file goes.
	g.Run("mkdir -p /tmp/werft-gl && touch /tmp/werft-gl/stale")
	var out bytes.Buffer
	err = guest.Copy(ctx, sh, addr, "/tmp/werft-gl", []guest.File{
		{Name: "helper", Data: []byte("h\n"), Mode: 0o755},
		{Name: "checks", Source: checks},
	}, &out)
	if err != nil {
		t.Fatalf("copy: %v\n%s", err, out.String())
	}
	got := g.Run("cd /tmp/werft-gl && ls -A && stat -c '%a %n' helper checks/sub/run.sh && sh checks/sub/run.sh")
	want := "checks\nhelper\n755 helper\n755 checks/sub/run.sh\nPASS\n"
	if got != want {
		t.Errorf("the guest dir holds\n%s\nwant\n%s", got, want)
	}
}
