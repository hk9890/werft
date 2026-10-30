package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/ansi"
	"github.com/hk9890/werft/internal/config"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/recipe"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/state"
	"github.com/hk9890/werft/internal/steps"
)

func (c *cli) cmdProvision(args []string) error {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "reset a running build VM and replace <name>-full without asking")
	recipePath := fs.String("recipe", "", "the recipe file; default: the recipe config.toml names for <name>")
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

	r, done, err := c.buildVMRun(name, *yes, io.MultiWriter(c.stderr, ansi.Strip(log)))
	if err != nil {
		return err
	}
	defer done()
	r.LogPath = logPath
	r.PasswordSpec = passwordSpec(rec.GuestPassword, r.User, name)
	r.Printf("Provisioning %s with the recipe %s; log: %s", name, rec.Path, logPath)
	if err := pipeline.Execute(r, steps.FromRecipe(rec)); err != nil {
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
	yes := fs.Bool("yes", false, "retake an existing provision-base without asking")
	if err := c.parseFlags(fs, "prepare", args); err != nil {
		return err
	}
	name, err := buildVMArg(fs, "prepare")
	if err != nil {
		return err
	}
	var spec secret.Spec
	if path, err := configuredRecipe(name); err != nil {
		return err
	} else if path != "" {
		if spec, err = recipe.GuestPassword(path); err != nil {
			return err
		}
	}
	dir, err := state.Root()
	if err != nil {
		return err
	}
	if err := guestEnsureKey(dir.SSHKey()); err != nil {
		return failure.Wrap(failure.Failed, err, "create the werft SSH key")
	}
	r, done, err := c.buildVMRun(name, *yes, c.stderr)
	if err != nil {
		return err
	}
	defer done()
	r.PasswordSpec = passwordSpec(spec, r.User, name)
	if err := pipeline.Execute(r, steps.Prepare()); err != nil {
		return err
	}
	r.Printf("%s is prepared: werft provision %s", name, name)
	c.output(map[string]string{"domain": name, "reset_point": steps.ProvisionBase}, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "%s is prepared\n", name)
	})
	return nil
}

func buildVMArg(fs *flag.FlagSet, command string) (string, error) {
	if fs.NArg() != 1 {
		return "", failure.New(failure.Usage, "%s takes one build VM name", command)
	}
	name := fs.Arg(0)
	if base, ok := strings.CutSuffix(name, steps.FullSuffix); ok {
		return "", failure.New(failure.Usage, "%s is a result image; %s its build VM, %s", name, command, base)
	}
	return name, nil
}

func configuredRecipe(name string) (string, error) {
	path, err := config.Path()
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", failure.Wrap(failure.Usage, err, "config")
	}
	return cfg.RecipeFor(name), nil
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
