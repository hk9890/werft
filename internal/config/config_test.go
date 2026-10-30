package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/config"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    config.Config
		wantErr string
	}{
		{"missing file", filepath.Join(t.TempDir(), "none.toml"), config.Default(), ""},
		{"empty file", write(t, ""), config.Default(), ""},
		{"pool set", write(t, `pool = "lab"`), config.Config{Pool: "lab"}, ""},
		{"unknown key", write(t, "pool = \"lab\"\nlimit = 3\n"), config.Config{}, "unknown keys: limit"},
		{"empty pool", write(t, `pool = ""`), config.Config{}, "pool is empty"},
		{"syntax error", write(t, `pool = `), config.Config{}, "read "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.Load(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Load = %+v, %v", got, err)
			}
		})
	}
}

func TestRecipeFor(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	path := write(t, "recipe = \"~/setup/recipe.toml\"\n[recipes]\nomarchy-4-0 = \"omarchy.toml\"\nubuntu-24-04 = \"/abs.toml\"\n")
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for vm, want := range map[string]string{
		"ubuntu-26-04": "/home/u/setup/recipe.toml",
		"omarchy-4-0":  filepath.Join(filepath.Dir(path), "omarchy.toml"),
		"ubuntu-24-04": "/abs.toml",
	} {
		if got := c.RecipeFor(vm); got != want {
			t.Errorf("RecipeFor(%s) = %q, want %q", vm, got, want)
		}
	}
	if got := config.Default().RecipeFor("x"); got != "" {
		t.Errorf("default RecipeFor = %q", got)
	}
}

func TestPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, _ := config.Path(); p != "/xdg/werft/config.toml" {
		t.Errorf("Path = %q", p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if p, _ := config.Path(); p != "/home/u/.config/werft/config.toml" {
		t.Errorf("Path = %q", p)
	}
}
