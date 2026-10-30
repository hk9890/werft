package secret_test

import (
	"errors"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
)

type store map[string]string

func (s store) Show(entry string) (string, error) {
	if v, ok := s[entry]; ok {
		return v, nil
	}
	return "", errors.New("not in the store")
}

type term struct {
	value string
	err   error
	asked bool
}

func (t *term) ReadSecret(string) (string, error) { t.asked = true; return t.value, t.err }

func TestRead(t *testing.T) {
	spec := secret.Spec{Pass: "vm/guest-password", Prompt: "Password"}
	var notes []string
	note := func(s string) { notes = append(notes, s) }

	tm := &term{value: "typed"}
	v, err := secret.Read(spec, store{"vm/guest-password": "from pass"}, tm, note)
	if err != nil || v != "from pass" || tm.asked || notes[0] != "comes from pass (vm/guest-password)" {
		t.Errorf("pass: %q %v asked=%v notes=%q", v, err, tm.asked, notes)
	}

	v, err = secret.Read(spec, store{}, tm, note)
	if err != nil || v != "typed" || !tm.asked {
		t.Errorf("fallback: %q %v", v, err)
	}

	_, err = secret.Read(spec, store{}, &term{err: failure.New(failure.SecretUnavailable, "no terminal")}, note)
	if !failure.Is(err, failure.SecretUnavailable) {
		t.Errorf("no terminal: %v", err)
	}

	_, err = secret.Read(secret.Spec{Prompt: "Passphrase"}, store{}, &term{}, note)
	if !failure.Is(err, failure.SecretUnavailable) {
		t.Errorf("empty answer: %v", err)
	}
}
