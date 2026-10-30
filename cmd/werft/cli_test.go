package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
)

type result struct {
	code   int
	stdout string
	stderr string
}

func runCLI(stdin string, tty bool, args ...string) result {
	var out, errOut bytes.Buffer
	c := &cli{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut, tty: tty}
	code := c.run(args)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func TestExitCodeTable(t *testing.T) {
	want := map[failure.Kind]int{
		failure.Failed: 1, failure.Usage: 2, failure.NotFound: 3, failure.Conflict: 4,
		failure.NeedsConfirmation: 5, failure.NoCapacity: 6, failure.Timeout: 7,
		failure.ChecksFailed: 8, failure.Unsupported: 9, failure.SecretUnavailable: 10,
	}
	for kind, code := range want {
		if got := exitCode(failure.New(kind, "x")); got != code {
			t.Errorf("exit code of %s = %d, want %d", kind, got, code)
		}
	}
	if exitCode(nil) != 0 || exitCode(bytes.ErrTooLarge) != 1 {
		t.Error("nil must exit 0 and a kindless error 1")
	}
}

func TestVersion(t *testing.T) {
	r := runCLI("", false, "version")
	if r.code != 0 || r.stdout != "werft dev (unknown, unknown)\n" {
		t.Errorf("version = %+v", r)
	}
	r = runCLI("", false, "version", "--json")
	var v map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v["version"] != "dev" {
		t.Errorf("version --json = %q, %v", r.stdout, err)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"bogus"}, {"version", "--bogus"}, {"version", "extra"}, {"help", "bogus"}} {
		r := runCLI("", false, args...)
		if r.code != 2 || !strings.HasPrefix(r.stderr, "werft: ") || r.stdout != "" {
			t.Errorf("%q = %+v, want exit 2 and a message on stderr", args, r)
		}
	}
}

func TestJSONError(t *testing.T) {
	r := runCLI("", false, "--json", "bogus")
	var doc struct {
		Error struct{ Kind, Message string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 2 || doc.Error.Kind != "usage" || !strings.Contains(doc.Error.Message, "bogus") || r.stderr != "" {
		t.Errorf("--json bogus = %+v", r)
	}
}

func TestJSONFlagStopsAtDoubleDash(t *testing.T) {
	c := &cli{}
	rest := c.globalFlags([]string{"version", "--", "--json"})
	if c.json || len(rest) != 3 {
		t.Errorf("globalFlags took --json after --: json=%v rest=%q", c.json, rest)
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		r := runCLI("", false, args...)
		if r.code != 0 || !strings.Contains(r.stdout, "  version ") || !strings.Contains(r.stdout, "5   needs_confirmation") {
			t.Errorf("%q = %+v", args, r)
		}
	}
	for _, args := range [][]string{{"help", "version"}, {"version", "--help"}} {
		r := runCLI("", false, args...)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "usage: werft version\n") {
			t.Errorf("%q = %+v", args, r)
		}
	}
}

func TestHelpJSON(t *testing.T) {
	for _, args := range [][]string{{"--json", "help"}, {"--json", "-h"}} {
		r := runCLI("", false, args...)
		var doc struct {
			Commands  []struct{ Name, Args, Summary string }
			ExitCodes map[string]int `json:"exit_codes"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
			t.Fatalf("%q: stdout is no JSON: %q", args, r.stdout)
		}
		if r.code != 0 || len(doc.Commands) != len(commands) || doc.Commands[1].Name != "version" ||
			doc.ExitCodes["ok"] != 0 || doc.ExitCodes["needs_confirmation"] != 5 {
			t.Errorf("%q = %+v", args, doc)
		}
	}
	for _, args := range [][]string{{"--json", "help", "version"}, {"--json", "version", "-h"}} {
		r := runCLI("", false, args...)
		var doc struct{ Name, Summary string }
		if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
			t.Fatalf("%q: stdout is no JSON: %q", args, r.stdout)
		}
		if r.code != 0 || doc.Name != "version" || doc.Summary == "" {
			t.Errorf("%q = %+v", args, doc)
		}
	}
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		name  string
		yes   bool
		tty   bool
		input string
		want  failure.Kind
	}{
		{"--yes skips the question", true, false, "", ""},
		{"no terminal", false, false, "y\n", failure.NeedsConfirmation},
		{"terminal, yes", false, true, "y\n", ""},
		{"terminal, YES", false, true, "YES\n", ""},
		{"terminal, no", false, true, "n\n", failure.NeedsConfirmation},
		{"terminal, end of input", false, true, "", failure.NeedsConfirmation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errOut bytes.Buffer
			c := &cli{stdin: strings.NewReader(tt.input), stderr: &errOut, tty: tt.tty}
			err := c.confirm(tt.yes, "reset vm1")
			if got := failure.KindOf(err); got != tt.want {
				t.Fatalf("confirm = %v, want kind %q", err, tt.want)
			}
			if !tt.tty && err != nil && !strings.Contains(err.Error(), "--yes") {
				t.Errorf("error does not name --yes: %v", err)
			}
			if asked := strings.Contains(errOut.String(), "[y/N]"); asked != (tt.tty && !tt.yes) {
				t.Errorf("prompt shown = %v", asked)
			}
		})
	}
}
