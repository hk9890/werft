// Package config loads ~/.config/werft/config.toml. The file is optional; a
// key it does not set keeps its default.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Pool is the libvirt storage pool for stock images and lease overlays.
	Pool string `toml:"pool"`
	// Recipe is the provision recipe of every build VM that Recipes does not
	// name.
	Recipe string `toml:"recipe"`
	// Recipes maps a build VM to its provision recipe.
	Recipes map[string]string `toml:"recipes"`
}

// RecipeFor is the recipe path of build VM vm, empty when there is none.
func (c Config) RecipeFor(vm string) string {
	if path, ok := c.Recipes[vm]; ok {
		return path
	}
	return c.Recipe
}

// PoolPath is the directory werft gives the pool when it creates it.
func (c Config) PoolPath() string { return filepath.Join("/var/lib/libvirt/images", c.Pool) }

func Default() Config { return Config{Pool: "werft"} }

// Path is $XDG_CONFIG_HOME/werft/config.toml, else ~/.config/werft/config.toml.
func Path() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "werft", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "werft", "config.toml"), nil
}

// Load reads the file at path over the defaults. A missing file gives the
// defaults; a key werft does not know is an error, so a typo does not pass
// silently.
func Load(path string) (Config, error) {
	c := Default()
	md, err := toml.DecodeFile(path, &c)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("read %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if c.Pool == "" {
		return Config{}, fmt.Errorf("read %s: pool is empty", path)
	}
	c.Recipe = resolve(filepath.Dir(path), c.Recipe)
	for vm, p := range c.Recipes {
		c.Recipes[vm] = resolve(filepath.Dir(path), p)
	}
	return c, nil
}

// resolve expands a leading ~/ and makes a relative path relative to dir.
func resolve(dir, path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}
