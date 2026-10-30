// Package recipe loads and validates a provision recipe: a TOML file that
// lists the provision steps and their parameters (docs/DESIGN.md, "Recipe").
// Paths in it resolve relative to the recipe file. Every error comes before
// any step runs.
package recipe

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
)

// Recipe is a validated recipe. Every path in it is absolute.
type Recipe struct {
	Path          string
	GuestPassword secret.Spec
	Secrets       map[string]secret.Spec
	Steps         []Step
}

// Step is one step of a recipe; which fields it uses depends on its Type.
type Step struct {
	Type    string
	ID      string
	Family  string
	Label   string
	Files   []string
	Command string
	Secret  string
	Dir     string
	Undo    string
}

// Families are the guest families detect-guest tells apart.
var Families = []string{"ubuntu", "omarchy"}

// params are the keys each step type takes besides type, id and family;
// a key in required must be set.
var params = map[string]struct{ allowed, required []string }{
	"reset":                 {allowed: []string{"label"}},
	"boot":                  {},
	"viewer":                {},
	"wait-ssh":              {},
	"verify-guest-password": {},
	"detect-guest":          {},
	"hostname":              {},
	"luks-mark":             {},
	"autologin":             {},
	"upload":                {allowed: []string{"files"}},
	"wait-session":          {},
	"unlock-keyring":        {},
	"sudo-grant":            {},
	"usb-passthrough":       {allowed: []string{"label"}, required: []string{"label"}},
	"pinentry":              {allowed: []string{"secret"}, required: []string{"secret"}},
	"run":                   {allowed: []string{"command", "secret"}, required: []string{"command"}},
	"checks":                {allowed: []string{"dir", "command"}, required: []string{"dir"}},
	"reboot":                {},
	"poweroff":              {},
	"clone":                 {},
	"generalize":            {},
	"snapshot":              {allowed: []string{"label"}, required: []string{"label"}},
	"undo":                  {allowed: []string{"step"}, required: []string{"step"}},
}

// Undoable are the step types with a cleanup, which undo can run early.
var Undoable = []string{"viewer", "autologin", "sudo-grant", "usb-passthrough", "pinentry"}

type file struct {
	GuestPassword *secret.Spec           `toml:"guest_password"`
	Secrets       map[string]secret.Spec `toml:"secrets"`
	Steps         []map[string]any       `toml:"steps"`
}

// Load reads and validates the recipe at path.
func Load(path string) (*Recipe, error) {
	abs, err := filepath.Abs(ExpandHome(path))
	if err != nil {
		return nil, failure.Wrap(failure.Usage, err, "recipe %s", path)
	}
	var f file
	md, err := toml.DecodeFile(abs, &f)
	if os.IsNotExist(err) {
		return nil, failure.New(failure.NotFound, "recipe %s does not exist", abs)
	}
	if err != nil {
		return nil, failure.Wrap(failure.Usage, err, "recipe %s", abs)
	}
	r := &Recipe{Path: abs, Secrets: f.Secrets}
	if r.Secrets == nil {
		r.Secrets = map[string]secret.Spec{}
	}
	if err := r.parse(f, md); err != nil {
		return nil, failure.Wrap(failure.Usage, err, "recipe %s", abs)
	}
	return r, nil
}

func (r *Recipe) parse(f file, md toml.MetaData) error {
	var unknown []string
	for _, k := range md.Undecoded() {
		if len(k) > 0 && k[0] != "steps" {
			unknown = append(unknown, k.String())
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown keys: %s", strings.Join(unknown, ", "))
	}
	if f.GuestPassword != nil {
		r.GuestPassword = *f.GuestPassword
	}
	for name, s := range r.Secrets {
		if s.Pass == "" && s.Prompt == "" {
			return fmt.Errorf("secret %s needs pass or prompt", name)
		}
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("no steps")
	}
	for i, raw := range f.Steps {
		s, err := r.step(raw)
		if err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		r.Steps = append(r.Steps, s)
	}
	return r.validateOrder()
}

func (r *Recipe) step(raw map[string]any) (Step, error) {
	var s Step
	var err error
	str := func(key string) string {
		v, ok := raw[key]
		if !ok || err != nil {
			return ""
		}
		text, isString := v.(string)
		if !isString || text == "" {
			err = fmt.Errorf("%s must be a non-empty string", key)
		}
		return text
	}
	s.Type = str("type")
	if err != nil || s.Type == "" {
		return s, fmt.Errorf("needs a type")
	}
	p, ok := params[s.Type]
	if !ok {
		return s, fmt.Errorf("unknown type %q", s.Type)
	}
	for key := range raw {
		if key != "type" && key != "id" && key != "family" && !slices.Contains(p.allowed, key) {
			return s, fmt.Errorf("%s takes no key %q", s.Type, key)
		}
	}
	for _, key := range p.required {
		if _, ok := raw[key]; !ok {
			return s, fmt.Errorf("%s needs %s", s.Type, key)
		}
	}
	s.ID, s.Family, s.Label = str("id"), str("family"), str("label")
	s.Command, s.Secret, s.Undo = str("command"), str("secret"), str("step")
	if dir := str("dir"); dir != "" {
		s.Dir = r.resolve(dir)
	}
	if err != nil {
		return s, err
	}
	if v, ok := raw["files"]; ok {
		list, isList := v.([]any)
		if !isList {
			return s, fmt.Errorf("files must be a list of paths")
		}
		for _, item := range list {
			name, isString := item.(string)
			if !isString || name == "" {
				return s, fmt.Errorf("files must be a list of paths")
			}
			s.Files = append(s.Files, r.resolve(name))
		}
	}
	if s.Family != "" && !slices.Contains(Families, s.Family) {
		return s, fmt.Errorf("unknown family %q; known: %s", s.Family, strings.Join(Families, ", "))
	}
	if _, ok := r.Secrets[s.Secret]; s.Secret != "" && !ok {
		return s, fmt.Errorf("unknown secret %q; declare it under [secrets.%s]", s.Secret, s.Secret)
	}
	for _, path := range append(slices.Clone(s.Files), s.Dir) {
		if path == "" {
			continue
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return s, fmt.Errorf("%s: %w", s.Type, statErr)
		}
	}
	if s.Dir != "" {
		if info, _ := os.Stat(s.Dir); !info.IsDir() {
			return s, fmt.Errorf("checks: %s is no directory", s.Dir)
		}
	}
	return s, nil
}

// validateOrder checks what depends on an earlier step: a family needs
// detect-guest before it, generalize a clone, and undo an earlier step with
// a cleanup.
func (r *Recipe) validateOrder() error {
	detected, cloned := false, false
	ids := map[string]bool{}
	for i, s := range r.Steps {
		if s.Type == "generalize" && !cloned {
			return fmt.Errorf("step %d: generalize needs a clone step before it", i+1)
		}
		if s.Family != "" && !detected {
			return fmt.Errorf("step %d (%s): family %s needs a detect-guest step before it", i+1, s.Type, s.Family)
		}
		if s.Type == "undo" && !ids[s.Undo] {
			return fmt.Errorf("step %d: undo names %q, which is no earlier step of type %s, nor its id", i+1, s.Undo, strings.Join(Undoable, ", "))
		}
		detected = detected || s.Type == "detect-guest"
		cloned = cloned || s.Type == "clone"
		if slices.Contains(Undoable, s.Type) {
			ids[s.Name()] = true
		}
	}
	return nil
}

// Name is the ID of the step, or its type when it has none.
func (s Step) Name() string {
	if s.ID != "" {
		return s.ID
	}
	return s.Type
}

func (r *Recipe) resolve(path string) string {
	path = ExpandHome(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(r.Path), path)
}

// ExpandHome replaces a leading ~/ with the home directory.
func ExpandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}
