package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompletionNamesEveryCommand(t *testing.T) {
	r := runCLI("", false, "completion", "zsh")
	if r.code != 0 {
		t.Fatalf("completion zsh = %+v", r)
	}
	for _, cmd := range commands {
		if !strings.Contains(r.stdout, "\n    '"+cmd.name+":") {
			t.Errorf("the script does not offer command %s", cmd.name)
		}
	}
}

func TestCompletionArguments(t *testing.T) {
	for _, args := range [][]string{{"completion"}, {"completion", "bash"}, {"completion", "zsh", "zsh"}} {
		if r := runCLI("", false, args...); r.code != 2 {
			t.Errorf("%q = %+v, want usage", args, r)
		}
	}
	bad := command{name: "x", args: "<unknown>"}
	if _, err := zshArguments(bad); err == nil {
		t.Error("an argument without a completer must fail")
	}
}

// TestZshCompletes types into an interactive zsh and records the candidates
// that the completion offers. A fake werft on PATH prints the JSON the real
// commands print against the fake hypervisor.
func TestZshCompletes(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil && os.Getenv("CI") != "" {
		t.Fatal("zsh is not installed; .github/workflows/ci.yml installs it")
	}
	if err != nil {
		t.Skip("zsh is not installed")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "werft.zsh")
	r := runCLI("", false, "completion", "zsh")
	if err := os.WriteFile(script, []byte(r.stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(zsh, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("zsh -n: %v\n%s", err, out)
	}

	bin := fakeWerft(t, dir)
	tests := []struct {
		input string
		want  []string
	}{
		{"werft ", commandNames()},
		{"werft reset ", []string{oddVM, "vm1"}},
		{"werft reset vm1 ", []string{"clean-install", "provision-base"}},
		{"werft reset --json vm1 ", []string{"clean-install", "provision-base"}},
		{"werft reset " + shQuote(oddVM) + " ", []string{oddLabel}},
		{"werft --json seal --yes vm1 ", []string{"clean-install", "provision-base"}},
		{"werft lease --json ", []string{"ubuntu-26-04"}},
		{"werft lease ubuntu-26-04 --tier ", []string{"full", "plain"}},
		{"werft release ", []string{"abc123"}},
		{"werft help ", commandNames()},
	}
	for _, tt := range tests {
		got := zshCandidates(t, zsh, script, bin, tt.input)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%q<TAB> offers %q, want %q", tt.input, got, tt.want)
		}
	}
}

// oddVM and oddLabel hold the characters JSON escapes, and & that Go's
// HTML escaping would turn into \u0026.
const (
	oddVM    = `q"a\b&c`
	oddLabel = `x\y"z&`
)

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func commandNames() []string {
	names := make([]string, len(commands))
	for i, cmd := range commands {
		names[i] = cmd.name
	}
	slices.Sort(names)
	return names
}

// fakeWerft writes a werft script into dir/bin that prints the JSON of
// list, snapshots of vm1 and oddVM, images and leases, made by the real
// commands. The lease VM is in the list.
func fakeWerft(t *testing.T, dir string) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	setClock(t, time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC))
	lh := newLifecycleHost(t)
	for _, label := range []string{"clean-install", "provision-base"} {
		if err := lh.fake.CreateSnapshot("vm1", label, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lh.fake.DefineDomain(`<domain type='kvm'><name>q&quot;a\b&amp;c</name></domain>`); err != nil {
		t.Fatal(err)
	}
	if err := lh.fake.CreateSnapshot(oddVM, oddLabel, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "werft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "werft", "images.toml"), []byte(imagesTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	addLease(t, lh, "abc123", time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC))

	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	var fake strings.Builder
	fake.WriteString("#!/bin/sh\ncase \"$*\" in\n")
	for i, args := range [][]string{{"--json", "list"}, {"--json", "snapshots", "vm1"}, {"--json", "snapshots", oddVM}, {"--json", "images"}, {"--json", "leases"}} {
		r := lh.run(args...)
		if r.code != 0 {
			t.Fatalf("werft %q = %+v", args, r)
		}
		file := filepath.Join(dir, fmt.Sprintf("%d.json", i))
		if err := os.WriteFile(file, []byte(r.stdout), 0o600); err != nil {
			t.Fatal(err)
		}
		fake.WriteString(shQuote(strings.Join(args, " ")) + ") cat " + shQuote(file) + " ;;\n")
	}
	fake.WriteString("esac\n")
	if err := os.WriteFile(filepath.Join(bin, "werft"), []byte(fake.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// zshCandidatesHarness runs an interactive zsh in a pseudo terminal, types
// the input and a TAB, and appends every candidate the completion adds to
// the file $4.
const zshCandidatesHarness = `
zmodload zsh/zpty
zpty z zsh -f -i
zpty -w z "PS1=''; PATH=${(q)2}:\$PATH; autoload -Uz compinit; compinit -u -D; source ${(q)1}"
zpty -w z "compadd() { local -a m; builtin compadd -O m \"\$@\"; print -rl -- \$m >> ${(q)4}; builtin compadd \"\$@\" }"
zpty -wn z "$3"$'\t'
for i in {1..100}; do
  [[ -s $4 ]] && break
  sleep 0.1
done
sleep 0.5
zpty -d z
`

func zshCandidates(t *testing.T, zsh, script, bin, input string) []string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "candidates")
	if err := os.WriteFile(out, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := exec.Command(zsh, "-f", "-c", zshCandidatesHarness, "harness", script, bin, input, out).CombinedOutput(); err != nil {
		t.Fatalf("zsh harness: %v\n%s", err, msg)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	slices.Sort(got)
	return slices.Compact(got)
}
