package secret_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
)

// store holds pass entries by name and keyring items by their attributes.
type store map[string]string

func (s store) Lookup(attributes map[string]string) (string, error) {
	return s.Show(secret.Attributes(attributes))
}

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
	spec := secret.Spec{Pass: "werft/guest-password", Prompt: "Password"}
	var notes []string
	note := func(s string) { notes = append(notes, s) }

	tm := &term{value: "typed"}
	v, err := secret.Read(spec, store{"werft/guest-password": "from pass"}, tm, note)
	if err != nil || v != "from pass" || tm.asked || notes[0] != "comes from pass (werft/guest-password)" {
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

func TestAnEmptyPassEntryIsAskedFor(t *testing.T) {
	tm := &term{value: "typed"}
	v, err := secret.Read(secret.Spec{Pass: "werft/guest-password", Prompt: "Password"}, store{"werft/guest-password": ""}, tm, func(string) {})
	if err != nil || v != "typed" || !tm.asked {
		t.Errorf("Read = %q, %v, asked %v", v, err, tm.asked)
	}
}

func TestReadTriesTheKeyringFirst(t *testing.T) {
	spec := secret.Spec{Keyring: map[string]string{"service": "werft", "username": "tester"}, Pass: "werft/guest-password", Prompt: "Password"}
	var notes []string
	note := func(s string) { notes = append(notes, s) }

	both := store{"service=werft username=tester": "from keyring", "werft/guest-password": "from pass"}
	if v, err := secret.Read(spec, both, &term{}, note); err != nil || v != "from keyring" || notes[0] != "comes from the keyring (service=werft username=tester)" {
		t.Errorf("keyring: %q %v notes=%q", v, err, notes)
	}

	notes = nil
	if v, err := secret.Read(spec, store{"werft/guest-password": "from pass"}, &term{}, note); err != nil || v != "from pass" {
		t.Errorf("pass after the keyring: %q %v", v, err)
	}

	notes = nil
	_, err := secret.Read(spec, store{}, &term{err: failure.New(failure.SecretUnavailable, "no terminal")}, note)
	want := "the keyring (service=werft username=tester) and pass (werft/guest-password) cannot give it"
	if !failure.Is(err, failure.SecretUnavailable) || !strings.HasPrefix(err.Error(), want) || notes[0] != want+", so it is asked for" {
		t.Errorf("none: %v notes=%q", err, notes)
	}
}
