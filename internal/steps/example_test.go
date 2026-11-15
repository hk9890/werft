package steps_test

import (
	"path/filepath"
	"testing"

	"github.com/hk9890/werft/internal/recipe"
	"github.com/hk9890/werft/internal/steps"
)

func TestExampleRecipeProvisions(t *testing.T) {
	rec, err := recipe.Load(filepath.Join("..", "..", "examples", "recipe.toml"))
	must(t, err)
	w := newWorld(t)
	r, err := w.run(steps.FromRecipe(rec), false)
	if err != nil {
		t.Fatalf("provision: %v\n%s", err, w.out.String())
	}
	if r.Domain != "testvm-full" {
		t.Errorf("the run ends on %s, not the clone", r.Domain)
	}
	w.assertSequence("the example installs its packages, then clones and snapshots",
		"^RevertSnapshot testvm provision-base$", "^StartDomain testvm$",
		"apt-get install -y git curl$",
		"^ssh sudo .*rm -f .* sh '/etc/sudoers.d/90-werft-provision'$",
		"systemctl poweroff", "^DefineDomain testvm-full$", "^CreateSnapshot testvm-full clean$")
	if w.count("pacman") != 0 {
		t.Error("an Ubuntu guest ran the Omarchy step")
	}
}
