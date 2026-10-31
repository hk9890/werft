package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SSHProxy connects ssh to an AF_VSOCK address.
const SSHProxy = "/usr/lib/systemd/systemd-ssh-proxy"

// OpenSSH runs commands with the OpenSSH client and the ssh_config at
// Config, which WriteConfig writes. The user's own ssh_config never applies,
// so its SendEnv LC_* does not hand the host's locale to a guest without it.
// KeyOnly offers the werft key alone, so a login tells whether the guest
// accepts it.
type OpenSSH struct {
	Config  string
	KeyOnly bool
}

// LoginPause is the pause after the attempt-th SSH login of a wait that
// failed: 1s, then doubling up to 8s. The attempts stay few on purpose: sshd
// (PerSourcePenalties, OpenSSH 9.8 and later) refuses every connection from
// a source whose failed and unauthenticated ones add up.
func LoginPause(attempt int) time.Duration {
	return time.Second << min(attempt-1, 3)
}

// keyOnlyTag selects the werft key alone in the werft ssh_config.
const keyOnlyTag = "werft-key-only"

func (o OpenSSH) Run(ctx context.Context, addr Addr, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, "ssh", o.Args(addr, cmd)...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	// When ctx ends, ssh is killed; a child of it, such as the proxy, can
	// still hold its output open. Wait gives up on that after the delay.
	c.WaitDelay = time.Second
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() != 255 {
		return &ExitError{Code: exit.ExitCode()}
	}
	if err != nil {
		return fmt.Errorf("ssh %s: %w", addr, err)
	}
	return nil
}

// Args are the ssh arguments that run cmd at addr; without cmd they open a
// login shell.
func (o OpenSSH) Args(addr Addr, cmd string) []string {
	args := []string{"-F", o.Config}
	if o.KeyOnly {
		args = append(args, "-P", keyOnlyTag)
	}
	args = append(args, addr.String())
	if cmd != "" {
		args = append(args, "--", cmd)
	}
	return args
}

// Command is the ssh command line a person runs to reach addr.
func (o OpenSSH) Command(addr Addr) string {
	return "ssh " + strings.Join(o.Args(addr, ""), " ")
}

// WriteConfig writes the werft ssh_config to path: it offers the werft key
// at key, then userKeys, and no other key of the SSH agent. Logins are few
// on purpose: each refused key counts toward sshd's MaxAuthTries, and sshd
// (PerSourcePenalties) resets every connection from a source whose failed
// ones add up. Host keys are not checked: a reset or a clone brings new ones
// on every run.
func WriteConfig(path, key string, userKeys ...string) error {
	var b strings.Builder
	b.WriteString("# Written by werft on every run.\n")
	b.WriteString("Host vsock/*\n")
	fmt.Fprintf(&b, "  ProxyCommand %s %%h %%p\n", SSHProxy)
	b.WriteString("  ProxyUseFdpass yes\n")
	b.WriteString("Host *\n")
	fmt.Fprintf(&b, "  IdentityFile %s\n", key)
	b.WriteString("  IdentitiesOnly yes\n")
	b.WriteString("  BatchMode yes\n")
	b.WriteString("  ConnectTimeout 5\n")
	b.WriteString("  StrictHostKeyChecking no\n")
	b.WriteString("  UserKnownHostsFile /dev/null\n")
	b.WriteString("  LogLevel ERROR\n")
	fmt.Fprintf(&b, "Match !tagged %s\n", keyOnlyTag)
	for _, k := range userKeys {
		fmt.Fprintf(&b, "  IdentityFile %s\n", k)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, b.String())
}

// writeAtomic replaces path as a whole: an ssh of another werft run that
// reads it meanwhile gets the old file or the new one, never a part.
func writeAtomic(path, content string) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// EnsureKey creates the ed25519 key pair at path unless it exists.
func EnsureKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "werft", "-f", path).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
