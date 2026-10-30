package config_test

import (
	"os"
	"path/filepath"
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
			if err != nil || got != tt.want {
				t.Errorf("Load = %+v, %v", got, err)
			}
		})
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
