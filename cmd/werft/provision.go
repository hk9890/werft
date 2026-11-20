package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/ansi"
	"github.com/hk9890/werft/internal/config"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/recipe"
	"github.com/hk9890/werft/internal/resetpoint"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/steps"
)

func (c *cli) cmdProvision(args []string) error {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "reset a running build VM and replace <name>-full without asking")
	fs.BoolVar(&yes, "y", false, "short for --yes")
	recipePath := fs.String("recipe", "", "the recipe file; default: the recipe config.toml names for <name>")
	key := fs.String("key", "", "the mount point of the stick the usb-passthrough step hands over; default: its label under /media/$USER or /run/media/$USER")
	if err := c.parseFlags(fs, "provision", args); err != nil {
		return err
	}
	name, err := buildVMArg(fs, "provision")
	if err != nil {
		return err
	}
	path := *recipePath
	if path == "" {
		if path, err = configuredRecipe(name); err != nil {
			return err
		}
		if path == "" {
			return failure.New(failure.Usage, "no recipe for %s: pass --recipe, or set recipe in %s", name, configPathHint())
		}
	}
	rec, err := recipe.Load(path)
	if err != nil {
		return err
	}
	if *key != "" && !slices.ContainsFunc(rec.Steps, func(s recipe.Step) bool { return s.Type == "usb-passthrough" }) {
		return failure.New(failure.Usage, "--key: the recipe %s has no usb-passthrough step", rec.Path)
	}

	dir, err := state.Root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir.Logs(), 0o700); err != nil {
		return failure.Wrap(failure.Failed, err, "create %s", dir.Logs())
	}
	logPath := filepath.Join(dir.Logs(), fmt.Sprintf("provision-%s-%s.log", name, time.Now().Format("20060102-150405")))
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return failure.Wrap(failure.Failed, err, "open %s", logPath)
	}
	defer func() { _ = log.Close() }()

	plainLog := ansi.Strip(log)
	r, done, err := c.buildVMRun(name, yes, io.MultiWriter(c.stderr, plainLog))
	if err != nil {
		return err
	}
	defer done()
	r.LogPath = logPath
	r.KeyMount = *key
	r.PasswordSpec = passwordSpec(rec.GuestPassword, r.User, name)
	r.Printf("Provisioning %s with the recipe %s; log: %s", name, rec.Path, logPath)
	if err := pipeline.Execute(r, steps.FromRecipe(rec)); err != nil {
		// The log ends with the failure, as stderr does.
		_, _ = fmt.Fprintf(plainLog, "werft: %v\n", err)
		return err
	}
	r.Printf("%s is ready", r.Domain)
	c.output(map[string]string{"build_vm": name, "domain": r.Domain, "log": logPath}, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "%s is ready\n", r.Domain)
	})
	return nil
}

func (c *cli) cmdPrepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "retake an existing provision-base without asking")
	fs.BoolVar(&yes, "y", false, "short for --yes")
	recipePath := fs.String("recipe", "", "the recipe whose guest_password prepare uses; default: the recipe config.toml names for <name>")
	if err := c.parseFlags(fs, "prepare", args); err != nil {
		return err
	}
	name, err := buildVMArg(fs, "prepare")
	if err != nil {
		return err
	}
	path := *recipePath
	if path == "" {
		if path, err = configuredRecipe(name); err != nil {
			return err
		}
	}
	var spec secret.Spec
	if path == "" {
		c.progress("No recipe for %s, so no keyring item or pass entry for the guest password; it is asked for at the terminal", name)
	} else if spec, err = recipe.GuestPassword(path); err != nil {
		return err
	}
	dir, err := state.Root()
	if err != nil {
		return err
	}
	if err := ensureKey(dir); err != nil {
		return failure.Wrap(failure.Failed, err, "create the werft SSH key")
	}
	r, done, err := c.buildVMRun(name, yes, c.stderr)
	if err != nil {
		return err
	}
	defer done()
	r.PasswordSpec = passwordSpec(spec, r.User, name)
	if err := pipeline.Execute(r, steps.Prepare()); err != nil {
		return err
	}
	r.Printf("%s is prepared: werft provision %s", name, name)
	c.output(map[string]string{"domain": name, "reset_point": resetpoint.ProvisionBase}, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "%s is prepared\n", name)
	})
	return nil
}

// ensureKey creates the werft key under the global lock: two first prepares
// on different build VMs would otherwise write one key pair at once.
func ensureKey(dir state.Dir) error {
	lock, err := dir.LockGlobal()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	return guestEnsureKey(dir.SSHKey())
}

func buildVMArg(fs *flag.FlagSet, command string) (string, error) {
	if fs.NArg() > 1 && slices.ContainsFunc(fs.Args()[1:], func(a string) bool { return strings.HasPrefix(a, "-") }) {
		return "", failure.New(failure.Usage, "flags go before the name: werft %s [flags] <name>", command)
	}
	if fs.NArg() != 1 {
		return "", failure.New(failure.Usage, "%s takes one build VM name", command)
	}
	name := fs.Arg(0)
	if base, ok := strings.CutSuffix(name, resetpoint.FullSuffix); ok {
		return "", failure.New(failure.Usage, "%s is a result image; %s its build VM, %s", name, command, base)
	}
	return name, nil
}

func configuredRecipe(name string) (string, error) {
	cfg, err := loadConfig()
	return cfg.RecipeFor(name), err
}

func loadConfig() (config.Config, error) {
	path, err := config.Path()
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, failure.Wrap(failure.Usage, err, "config")
	}
	return cfg, nil
}

func configPathHint() string {
	path, err := config.Path()
	if err != nil {
		return "config.toml"
	}
	return path
}

// passwordSpec fills in the prompt of the guest password.
func passwordSpec(spec secret.Spec, user, name string) secret.Spec {
	if spec.Prompt == "" {
		spec.Prompt = fmt.Sprintf("Password of %s in the VM %s, for its sudo", user, name)
	}
	return spec
}
