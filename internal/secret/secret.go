// Package secret reads a secret from the keyring, else from pass, else from
// a prompt at the terminal, else fails with failure.SecretUnavailable. A
// secret stays in memory and reaches a guest only as stdin data.
package secret

import (
	"context"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/failure"
)

// Spec says where a secret comes from: the first line of the keyring item
// with the attributes Keyring, when set, else of pass entry Pass, when set,
// else the answer to Prompt at the terminal.
type Spec struct {
	Keyring map[string]string `toml:"keyring"`
	Pass    string            `toml:"pass"`
	Prompt  string            `toml:"prompt"`
}

// Store gives the first line of a keyring item or of a pass entry.
type Store interface {
	Lookup(attributes map[string]string) (string, error)
	Show(entry string) (string, error)
}

// Terminal asks a question without echo. It fails with
// failure.SecretUnavailable when there is no terminal.
type Terminal interface {
	ReadSecret(prompt string) (string, error)
}

// Attributes is attrs as sorted name=value pairs. Attributes name an item,
// never its value.
func Attributes(attrs map[string]string) string {
	pairs := make([]string, 0, len(attrs))
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		pairs = append(pairs, k+"="+attrs[k])
	}
	return strings.Join(pairs, " ")
}

// HasSource says whether spec names a keyring item or a pass entry.
func (s Spec) HasSource() bool {
	return len(s.Keyring) > 0 || s.Pass != ""
}

type source struct {
	name string
	read func() (string, error)
}

func sources(spec Spec, store Store) []source {
	var s []source
	if len(spec.Keyring) > 0 {
		s = append(s, source{"the keyring (" + Attributes(spec.Keyring) + ")", func() (string, error) { return store.Lookup(spec.Keyring) }})
	}
	if spec.Pass != "" {
		s = append(s, source{"pass (" + spec.Pass + ")", func() (string, error) { return store.Show(spec.Pass) }})
	}
	return s
}

// Read gives the secret of spec. note gets one line that says where the
// secret came from; the value never shows.
func Read(spec Spec, store Store, term Terminal, note func(string)) (string, error) {
	var failed []string
	for _, s := range sources(spec, store) {
		if v, err := s.read(); err == nil && v != "" {
			note("comes from " + s.name)
			return v, nil
		}
		failed = append(failed, s.name)
	}
	if len(failed) > 0 {
		note(strings.Join(failed, " and ") + " cannot give it, so it is asked for")
	}
	v, err := term.ReadSecret(spec.Prompt)
	if err != nil && len(failed) > 0 {
		return "", failure.Wrap(failure.KindOf(err), err, "%s cannot give it", strings.Join(failed, " and "))
	}
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", failure.New(failure.SecretUnavailable, "no value given for %q", spec.Prompt)
	}
	return v, nil
}

// Host is the Store of the host: the Secret Service through secret-tool, and
// the pass command. stdin is closed, so neither can eat the terminal; a
// pinentry pass needs for the GPG key comes from gpg-agent.
type Host struct{}

// Lookup asks only an unlocked keyring: secret-tool on a locked one opens an
// unlock dialog, and an agent cannot answer it. toolTimeout ends a dialog
// that opens anyway.
func (Host) Lookup(attributes map[string]string) (string, error) {
	if !keyringUnlocked() {
		return "", failure.New(failure.SecretUnavailable, "the keyring is locked or does not run")
	}
	args := []string{"lookup"}
	for _, k := range slices.Sorted(maps.Keys(attributes)) {
		args = append(args, k, attributes[k])
	}
	return firstLine("secret-tool", args...)
}

func (Host) Show(entry string) (string, error) { return firstLine("pass", "show", entry) }

// keyringUnlocked asks the session bus, not the Secret Service: a call to
// the service would start it, and a keyring started that way stays locked.
func keyringUnlocked() bool {
	owner, err := output("busctl", "--user", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus",
		"NameHasOwner", "s", "org.freedesktop.secrets")
	if err != nil || strings.TrimSpace(owner) != "b true" {
		return false
	}
	locked, err := output("busctl", "--user", "get-property", "org.freedesktop.secrets", "/org/freedesktop/secrets/aliases/default",
		"org.freedesktop.Secret.Collection", "Locked")
	return err == nil && strings.TrimSpace(locked) == "b false"
}

func firstLine(name string, args ...string) (string, error) {
	out, err := output(name, args...)
	if err != nil {
		return "", failure.Wrap(failure.SecretUnavailable, err, "%s %s", name, strings.Join(args, " "))
	}
	first, _, _ := strings.Cut(out, "\n")
	return first, nil
}

// toolTimeout bounds each call of busctl, secret-tool and pass: a keyring
// can open a dialog that nobody answers, also past the unlock check, for an
// item in another, locked collection.
var toolTimeout = 30 * time.Second

// output runs name with stdin closed and gives its stdout.
func output(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}
