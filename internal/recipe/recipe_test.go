package recipe_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/recipe"
)

// write puts a recipe and a script and a check directory next to it.
func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "checks"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "recipe.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const valid = `
[guest_password]
pass = "vm/guest-password"

[secrets.passphrase]
prompt = "GPG passphrase"

[[steps]]
type = "reset"

[[steps]]
type = "detect-guest"

[[steps]]
type = "upload"
files = ["script.sh"]

[[steps]]
type = "sudo-grant"
id = "sudo"
family = "omarchy"

[[steps]]
type = "run"
command = "bash script.sh"
secret = "passphrase"

[[steps]]
type = "undo"
step = "sudo"

[[steps]]
type = "checks"
dir = "checks"

[[steps]]
type = "clone"

[[steps]]
type = "generalize"

[[steps]]
type = "snapshot"
label = "clean"
`

func TestLoad(t *testing.T) {
	path := write(t, valid)
	r, err := recipe.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if r.GuestPassword.Pass != "vm/guest-password" || r.Secrets["passphrase"].Prompt != "GPG passphrase" {
		t.Errorf("secrets = %+v %+v", r.GuestPassword, r.Secrets)
	}
	if len(r.Steps) != 10 {
		t.Fatalf("%d steps", len(r.Steps))
	}
	if got := r.Steps[2].Files; len(got) != 1 || got[0] != filepath.Join(dir, "script.sh") {
		t.Errorf("files resolve relative to the recipe: %q", got)
	}
	if r.Steps[6].Dir != filepath.Join(dir, "checks") {
		t.Errorf("dir = %q", r.Steps[6].Dir)
	}
	if s := r.Steps[3]; s.ID != "sudo" || s.Family != "omarchy" || s.Name() != "sudo" {
		t.Errorf("step 4 = %+v", s)
	}
}

func TestLoadRejects(t *testing.T) {
	steps := func(s string) string { return "[secrets.p]\nprompt = \"p\"\n" + s }
	tests := []struct{ name, content, want string }{
		{"no steps", "", "no steps"},
		{"unknown top key", "bogus = 1\n" + steps("[[steps]]\ntype = \"boot\"\n"), "unknown keys: bogus"},
		{"unknown secret key", "[secrets.x]\nprompt = \"x\"\nfile = \"y\"\n[[steps]]\ntype = \"boot\"\n", "unknown keys: secrets.x.file"},
		{"empty secret", "[secrets.x]\n[[steps]]\ntype = \"boot\"\n", "secret x needs pass or prompt"},
		{"no type", steps("[[steps]]\nlabel = \"x\"\n"), "step 1: needs a type"},
		{"unknown type", steps("[[steps]]\ntype = \"bogus\"\n"), `unknown type "bogus"`},
		{"key of another type", steps("[[steps]]\ntype = \"boot\"\nlabel = \"x\"\n"), `boot takes no key "label"`},
		{"missing required", steps("[[steps]]\ntype = \"run\"\n"), "run needs command"},
		{"wrong value type", steps("[[steps]]\ntype = \"run\"\ncommand = 3\n"), "command must be a non-empty string"},
		{"files not a list", steps("[[steps]]\ntype = \"upload\"\nfiles = \"script.sh\"\n"), "files must be a list"},
		{"missing file", steps("[[steps]]\ntype = \"upload\"\nfiles = [\"none.sh\"]\n"), "none.sh"},
		{"checks dir is a file", steps("[[steps]]\ntype = \"checks\"\ndir = \"script.sh\"\n"), "is no directory"},
		{"unknown secret", steps("[[steps]]\ntype = \"pinentry\"\nsecret = \"nope\"\n"), `unknown secret "nope"`},
		{"unknown family", steps("[[steps]]\ntype = \"detect-guest\"\n[[steps]]\ntype = \"boot\"\nfamily = \"debian\"\n"), `unknown family "debian"`},
		{"family before detect", steps("[[steps]]\ntype = \"boot\"\nfamily = \"ubuntu\"\n"), "needs a detect-guest step before it"},
		{"undo of nothing", steps("[[steps]]\ntype = \"undo\"\nstep = \"sudo-grant\"\n"), `undo names "sudo-grant"`},
		{"undo of a step without cleanup", steps("[[steps]]\ntype = \"boot\"\n[[steps]]\ntype = \"undo\"\nstep = \"boot\"\n"), `undo names "boot"`},
		{"generalize without clone", steps("[[steps]]\ntype = \"generalize\"\n"), "generalize needs a clone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := recipe.Load(write(t, tt.content))
			if !failure.Is(err, failure.Usage) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want usage error with %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := recipe.Load(filepath.Join(t.TempDir(), "none.toml"))
	if !failure.Is(err, failure.NotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	if got := recipe.ExpandHome("~/setup/tests"); got != "/home/u/setup/tests" {
		t.Errorf("ExpandHome = %q", got)
	}
	if got := recipe.ExpandHome("a/~/b"); got != "a/~/b" {
		t.Errorf("ExpandHome = %q", got)
	}
}
