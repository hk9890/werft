// Package secret reads a secret from pass, else from a prompt at the
// terminal, else fails with failure.SecretUnavailable. A secret stays in
// memory and reaches a guest only as stdin data.
package secret

import (
	"bytes"
	"os/exec"
	"strings"

	"github.com/hk9890/werft/internal/failure"
)

// Spec says where a secret comes from: the first line of pass entry Pass,
// when set, else the answer to Prompt at the terminal.
type Spec struct {
	Pass   string `toml:"pass"`
	Prompt string `toml:"prompt"`
}

// Store gives the first line of a pass entry.
type Store interface {
	Show(entry string) (string, error)
}

// Terminal asks a question without echo. It fails with
// failure.SecretUnavailable when there is no terminal.
type Terminal interface {
	ReadSecret(prompt string) (string, error)
}

// Read gives the secret of spec. note gets one line that says where the
// secret came from; the value never shows.
func Read(spec Spec, store Store, term Terminal, note func(string)) (string, error) {
	if spec.Pass != "" {
		if v, err := store.Show(spec.Pass); err == nil && v != "" {
			note("comes from pass (" + spec.Pass + ")")
			return v, nil
		}
		note("pass cannot give " + spec.Pass + ", so it is asked for")
	}
	v, err := term.ReadSecret(spec.Prompt)
	if err != nil && spec.Pass != "" {
		return "", failure.Wrap(failure.KindOf(err), err, "pass cannot give %s", spec.Pass)
	}
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", failure.New(failure.SecretUnavailable, "no value given for %q", spec.Prompt)
	}
	return v, nil
}

// Pass is the Store of the pass command. stdin is closed, so pass cannot eat
// the terminal; a pinentry it needs for the GPG key comes from gpg-agent.
type Pass struct{}

func (Pass) Show(entry string) (string, error) {
	var out bytes.Buffer
	c := exec.Command("pass", "show", entry)
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return "", failure.Wrap(failure.SecretUnavailable, err, "pass show %s", entry)
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	return first, nil
}
