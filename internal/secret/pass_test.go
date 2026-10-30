package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
)

// fakePass puts a pass on PATH that knows one entry. Its entry holds a
// second line, as pass entries carry metadata. It records what it read on
// stdin.
func fakePass(t *testing.T) (stdin string) {
	t.Helper()
	dir := t.TempDir()
	stdin = filepath.Join(dir, "stdin")
	script := `#!/bin/sh
cat >"` + stdin + `"
if [ "$2" != vm/guest-password ]; then
    echo "Error: $2 is not in the password store." >&2
    exit 1
fi
printf '  gu3st pw  \nlogin: stub\n'
`
	if err := os.WriteFile(filepath.Join(dir, "pass"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stdin
}

func TestPassGivesTheFirstLineWithItsSpaces(t *testing.T) {
	stdin := fakePass(t)
	v, err := secret.Pass{}.Show("vm/guest-password")
	if err != nil || v != "  gu3st pw  " {
		t.Fatalf("Show = %q, %v", v, err)
	}
	if data, err := os.ReadFile(stdin); err != nil || len(data) != 0 {
		t.Errorf("pass read %q from stdin, %v; stdin must be closed", data, err)
	}
}

func TestPassWithoutTheEntryFallsBackQuietly(t *testing.T) {
	fakePass(t)
	_, err := secret.Pass{}.Show("vm/other")
	if !failure.Is(err, failure.SecretUnavailable) {
		t.Fatalf("err = %v", err)
	}
	tm := &term{err: failure.New(failure.SecretUnavailable, "stdin is no terminal to ask on")}
	var notes []string
	_, err = secret.Read(secret.Spec{Pass: "vm/other", Prompt: "Password"}, secret.Pass{}, tm, func(s string) { notes = append(notes, s) })
	if !failure.Is(err, failure.SecretUnavailable) || err.Error() != "pass cannot give vm/other: stdin is no terminal to ask on" {
		t.Errorf("err = %v", err)
	}
	if len(notes) != 1 || notes[0] != "pass cannot give vm/other, so it is asked for" {
		t.Errorf("notes = %q", notes)
	}
}
