package secret_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
)

// fakeTool puts a program name on PATH that runs script. It returns the file
// that receives what the program read on stdin.
func fakeTool(t *testing.T, dir, name, script string) (stdin string) {
	t.Helper()
	stdin = filepath.Join(dir, name+".stdin")
	body := "#!/bin/sh\ncat >\"" + stdin + "\"\n" + script
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return stdin
}

func onPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// fakePass knows one entry. Its entry holds a second line, as pass entries
// carry metadata.
func fakePass(t *testing.T) (stdin string) {
	t.Helper()
	return fakeTool(t, onPath(t), "pass", `
if [ "$2" != werft/guest-password ]; then
    echo "Error: $2 is not in the password store." >&2
    exit 1
fi
printf '  gu3st pw  \nlogin: stub\n'
`)
}

// fakeKeyring is a session bus whose Secret Service runs, with the default
// collection locked or not, and a secret-tool that knows the item
// service=werft username=tester.
func fakeKeyring(t *testing.T, running, locked bool) (stdin string) {
	t.Helper()
	dir := onPath(t)
	owner, state := "b false", "b true"
	if running {
		owner = "b true"
	}
	if !locked {
		state = "b false"
	}
	fakeTool(t, dir, "busctl", `
case "$2" in
call) echo "`+owner+`" ;;
get-property) echo "`+state+`" ;;
*) exit 1 ;;
esac
`)
	return fakeTool(t, dir, "secret-tool", `
if [ "$*" != "lookup service werft username tester" ]; then
    exit 1
fi
printf 'k3yring pw'
`)
}

func TestPassGivesTheFirstLineWithItsSpaces(t *testing.T) {
	stdin := fakePass(t)
	v, err := secret.Host{}.Show("werft/guest-password")
	if err != nil || v != "  gu3st pw  " {
		t.Fatalf("Show = %q, %v", v, err)
	}
	if data, err := os.ReadFile(stdin); err != nil || len(data) != 0 {
		t.Errorf("pass read %q from stdin, %v; stdin must be closed", data, err)
	}
}

func TestPassWithoutTheEntryFallsBackQuietly(t *testing.T) {
	fakePass(t)
	_, err := secret.Host{}.Show("vm/other")
	if !failure.Is(err, failure.SecretUnavailable) {
		t.Fatalf("err = %v", err)
	}
	tm := &term{err: failure.New(failure.SecretUnavailable, "stdin is no terminal to ask on")}
	var notes []string
	_, err = secret.Read(secret.Spec{Pass: "vm/other", Prompt: "Password"}, secret.Host{}, tm, func(s string) { notes = append(notes, s) })
	if !failure.Is(err, failure.SecretUnavailable) || err.Error() != "pass (vm/other) cannot give it: stdin is no terminal to ask on" {
		t.Errorf("err = %v", err)
	}
	if len(notes) != 1 || notes[0] != "pass (vm/other) cannot give it, so it is asked for" {
		t.Errorf("notes = %q", notes)
	}
}

func TestKeyringGivesTheItem(t *testing.T) {
	stdin := fakeKeyring(t, true, false)
	v, err := secret.Host{}.Lookup(map[string]string{"username": "tester", "service": "werft"})
	if err != nil || v != "k3yring pw" {
		t.Fatalf("Lookup = %q, %v", v, err)
	}
	if data, err := os.ReadFile(stdin); err != nil || len(data) != 0 {
		t.Errorf("secret-tool read %q from stdin, %v; stdin must be closed", data, err)
	}
	if _, err := (secret.Host{}).Lookup(map[string]string{"service": "werft", "username": "other"}); !failure.Is(err, failure.SecretUnavailable) {
		t.Errorf("Lookup of a missing item = %v, want secret_unavailable", err)
	}
}

// TestKeyringNotUnlockedIsNotAsked: secret-tool on a locked keyring opens
// an unlock dialog, and a call to a Secret Service that does not run starts
// one that stays locked.
func TestKeyringNotUnlockedIsNotAsked(t *testing.T) {
	for name, c := range map[string]struct{ running, locked bool }{
		"locked":                  {true, true},
		"not running":             {false, true},
		"not running, not locked": {false, false},
	} {
		t.Run(name, func(t *testing.T) {
			stdin := fakeKeyring(t, c.running, c.locked)
			_, err := secret.Host{}.Lookup(map[string]string{"service": "werft", "username": "tester"})
			if !failure.Is(err, failure.SecretUnavailable) {
				t.Errorf("Lookup = %v, want secret_unavailable", err)
			}
			if _, err := os.Stat(stdin); err == nil {
				t.Error("secret-tool ran on a keyring that is not unlocked")
			}
		})
	}
}

// TestKeyringDialogTimesOut: a lookup that waits for a dialog nobody
// answers ends after the tool timeout.
func TestKeyringDialogTimesOut(t *testing.T) {
	fakeKeyring(t, true, false)
	dir := onPath(t)
	fakeTool(t, dir, "secret-tool", "exec sleep 5\n")
	defer func(d time.Duration) { *secret.ToolTimeout = d }(*secret.ToolTimeout)
	*secret.ToolTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err := secret.Host{}.Lookup(map[string]string{"service": "werft", "username": "tester"})
	if !failure.Is(err, failure.SecretUnavailable) || time.Since(start) > 3*time.Second {
		t.Errorf("Lookup = %v after %v, want secret_unavailable after the timeout", err, time.Since(start))
	}
}
