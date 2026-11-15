package steps_test

import (
	"path/filepath"
	"testing"

	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/recipe"
	"github.com/hk9890/werft/internal/steps"
)

func TestExampleRecipeProvisions(t *testing.T) {
	rec, err := recipe.Load(filepath.Join("..", "..", "examples", "recipe.toml"))
	must(t, err)
	w := newWorld(t)
	w.term.secrets[rec.GuestPassword.Prompt] = password
	r := w.newRun(false)
	r.PasswordSpec = rec.GuestPassword
	if err := pipeline.Execute(r, steps.FromRecipe(rec)); err != nil {
		t.Fatalf("provision: %v\n%s", err, w.out.String())
	}
	if r.Domain != "testvm-full" {
		t.Errorf("the run ends on %s, not the clone", r.Domain)
	}
	w.assertSequence("the example waits for apt, installs its packages, then clones and snapshots",
		"^secret Password of the guest user", "^RevertSnapshot testvm provision-base$", "^StartDomain testvm$",
		"wait_for_package_manager' && sudo apt-get update && .*apt-get install -y git curl$",
		"^ssh sudo .*rm -f .* sh '/etc/sudoers.d/90-werft-provision'$",
		"systemctl poweroff", "^DefineDomain testvm-full$", "^CreateSnapshot testvm-full clean$")
}
