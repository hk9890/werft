// Package state owns ~/.local/state/werft: the paths of what werft writes
// there, and the locks of docs/DESIGN.md, "Concurrency and locking".
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hk9890/werft/internal/failure"
)

// Dir is the state directory.
type Dir string

// Root is $XDG_STATE_HOME/werft, else ~/.local/state/werft.
func Root() (Dir, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return Dir(filepath.Join(x, "werft")), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return Dir(filepath.Join(home, ".local", "state", "werft")), nil
}

func (d Dir) Images() string         { return filepath.Join(string(d), "images.toml") }
func (d Dir) Leases() string         { return filepath.Join(string(d), "leases") }
func (d Dir) Lease(id string) string { return filepath.Join(d.Leases(), id) }
func (d Dir) Logs() string           { return filepath.Join(string(d), "logs") }
func (d Dir) Locks() string          { return filepath.Join(string(d), "locks") }

// Lock is a held flock. The kernel drops it when the process ends, so a
// crashed run leaves no stale lock.
type Lock struct{ f *os.File }

func (l *Lock) Release() error { return l.f.Close() }

// LockGlobal takes locks/global.lock and waits for it while another run
// holds it.
func (d Dir) LockGlobal() (*Lock, error) {
	return d.lock("global", true)
}

// LockDomain takes locks/<domain>.lock. It does not wait: while another run
// holds it, it fails with failure.Conflict.
func (d Dir) LockDomain(domain string) (*Lock, error) {
	if domain == "" || domain == "global" || strings.ContainsAny(domain, "/\x00") || strings.HasPrefix(domain, ".") {
		return nil, failure.New(failure.Usage, "no lock for domain name %q", domain)
	}
	return d.lock(domain, false)
}

func (d Dir) lock(name string, wait bool) (*Lock, error) {
	if err := os.MkdirAll(d.Locks(), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	path := filepath.Join(d.Locks(), name+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, failure.New(failure.Conflict, "another werft run works on %s", name)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}
