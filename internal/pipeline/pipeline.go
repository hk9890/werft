// Package pipeline runs the steps of prepare and provision (docs/DESIGN.md,
// "Provision pipeline"). Every step's check runs before any step runs, so all
// questions come before the first destructive action, and every secret is
// read after the last question. A step's cleanup is pushed before the step
// runs, and the stack is popped in reverse on success, on failure and on
// SIGINT.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virt"
)

// Step is one step of a pipeline.
type Step interface {
	// Check validates inputs, asks every question and declares the secrets
	// the step needs. It changes nothing.
	Check(r *Run) error
	Run(r *Run) error
}

// Cleaner is a step that undoes what it did. Cleanup must be safe to run
// twice, and after a run that stopped half-way.
type Cleaner interface {
	Cleanup(r *Run) error
}

// Entry is a step in a pipeline. ID names it for Undo; it defaults to Name.
// A step with a Family runs only in a guest of that family.
type Entry struct {
	Name   string
	ID     string
	Family string
	Step   Step
}

func (e Entry) id() string {
	if e.ID != "" {
		return e.ID
	}
	return e.Name
}

type pushed struct {
	id      string
	cleaner Cleaner
}

// Execute runs the entries. On failure it leaves the domain running and
// names the SSH command and the log in the error.
func Execute(r *Run, entries []Entry) error {
	for _, e := range entries {
		if err := e.Step.Check(r); err != nil {
			return err
		}
	}
	if err := r.readSecrets(); err != nil {
		return err
	}
	err := run(r, entries)
	r.Ctx = context.Background()
	if cerr := r.popAll(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return r.failed(err)
	}
	return nil
}

func run(r *Run, entries []Entry) error {
	for i, e := range entries {
		if e.Family != "" && e.Family != r.Family {
			r.Printf("[%d/%d] %s: skipped, %s is no %s guest", i+1, len(entries), e.Name, r.Domain, e.Family)
			continue
		}
		r.Printf("[%d/%d] %s", i+1, len(entries), e.Name)
		if c, ok := e.Step.(Cleaner); ok {
			r.stack = append(r.stack, pushed{id: e.id(), cleaner: c})
		}
		if err := e.Step.Run(r); err != nil {
			return failure.Wrap(failure.KindOf(err), err, "%s", e.Name)
		}
		if err := r.Ctx.Err(); err != nil {
			return failure.Wrap(failure.Failed, err, "interrupted after %s", e.Name)
		}
	}
	return nil
}

// popAll runs every cleanup left on the stack, newest first. A failed
// cleanup does not stop the others.
func (r *Run) popAll() error {
	var errs []error
	for len(r.stack) > 0 {
		p := r.stack[len(r.stack)-1]
		r.stack = r.stack[:len(r.stack)-1]
		if err := p.cleaner.Cleanup(r); err != nil {
			r.Printf("warning: the cleanup of %s failed: %v", p.id, err)
			errs = append(errs, fmt.Errorf("cleanup of %s: %w", p.id, err))
		}
	}
	return errors.Join(errs...)
}

// Undo runs the cleanup of the newest pushed step with ID now and takes it
// off the stack, so a temporary change ends where the recipe says. Nothing
// happens when no such step ran.
type Undo struct{ ID string }

func (Undo) Check(*Run) error { return nil }

func (u Undo) Run(r *Run) error {
	for i := len(r.stack) - 1; i >= 0; i-- {
		if r.stack[i].id != u.ID {
			continue
		}
		c := r.stack[i].cleaner
		r.stack = slices.Delete(r.stack, i, i+1)
		return c.Cleanup(r)
	}
	return nil
}

func (r *Run) failed(err error) error {
	var hint []string
	if s, serr := r.H.DomainState(r.Domain); serr == nil && s == virt.StateRunning {
		if r.Addr.Host != "" {
			hint = append(hint, fmt.Sprintf("%s is left running for inspection: %s", r.Domain, r.SSHCommand()))
		} else {
			hint = append(hint, r.Domain+" is left running for inspection")
		}
	}
	if r.LogPath != "" {
		hint = append(hint, "log: "+r.LogPath)
	}
	if len(hint) == 0 {
		return err
	}
	return failure.New(failure.KindOf(err), "%v\n%s", err, strings.Join(hint, "\n"))
}

// Declare says that the run needs the secret name, read as spec says. Each
// validate runs on the value once it is read. Declaring a name twice keeps
// the first spec.
func (r *Run) Declare(name string, spec secret.Spec, validate ...func(string) error) {
	if _, ok := r.specs[name]; !ok {
		r.specs[name] = spec
		r.order = append(r.order, name)
	}
	r.validators[name] = append(r.validators[name], validate...)
}

func (r *Run) readSecrets() error {
	for _, name := range r.order {
		v, err := secret.Read(r.specs[name], r.Secrets, r.Prompter, func(note string) {
			r.Printf("%s %s", name, note)
		})
		if err != nil {
			return err
		}
		for _, validate := range r.validators[name] {
			if err := validate(v); err != nil {
				return err
			}
		}
		r.values[name] = v
	}
	return nil
}

// Secret is the value of a declared secret, empty before it is read.
func (r *Run) Secret(name string) string { return r.values[name] }
