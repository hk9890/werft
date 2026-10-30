package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virttest"
)

type stub struct {
	name    string
	log     *[]string
	failRun bool
	declare string
	cancel  context.CancelFunc
}

func (s *stub) Check(r *pipeline.Run) error {
	*s.log = append(*s.log, "check "+s.name)
	if s.declare != "" {
		r.Declare(s.declare, secret.Spec{Prompt: s.declare})
	}
	return nil
}

func (s *stub) Run(r *pipeline.Run) error {
	*s.log = append(*s.log, "run "+s.name+" "+r.Secret(s.declare))
	if s.cancel != nil {
		s.cancel()
	}
	if s.failRun {
		return failure.New(failure.Timeout, "%s broke", s.name)
	}
	return nil
}

type cleaning struct{ stub }

func (c *cleaning) Cleanup(*pipeline.Run) error {
	*c.log = append(*c.log, "cleanup "+c.name)
	return nil
}

type prompter struct{ log *[]string }

func (p prompter) Confirm(q string) error { *p.log = append(*p.log, "confirm "+q); return nil }

func (p prompter) ReadSecret(prompt string) (string, error) {
	*p.log = append(*p.log, "secret "+prompt)
	return "v-" + prompt, nil
}

func newRun(log *[]string) *pipeline.Run {
	f := virttest.New()
	_, _ = f.DefineDomain("<domain type='kvm'><name>vm</name></domain>")
	return (&pipeline.Run{H: f, Out: &bytes.Buffer{}, Prompter: prompter{log}, Domain: "vm", BuildVM: "vm"}).Init()
}

func TestChecksAndSecretsComeBeforeAnyRun(t *testing.T) {
	var log []string
	r := newRun(&log)
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "a", Step: &stub{name: "a", log: &log, declare: "s1"}},
		{Name: "b", Step: &cleaning{stub{name: "b", log: &log}}},
		{Name: "c", Step: &stub{name: "c", log: &log, declare: "s1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"check a", "check b", "check c", "secret s1", "run a v-s1", "run b ", "run c v-s1", "cleanup b"}
	if !slices.Equal(log, want) {
		t.Errorf("log = %q\nwant  %q", log, want)
	}
}

func TestFailureStopsAndPopsCleanupsInReverse(t *testing.T) {
	var log []string
	r := newRun(&log)
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "a", Step: &cleaning{stub{name: "a", log: &log}}},
		{Name: "b", Step: &cleaning{stub{name: "b", log: &log, failRun: true}}},
		{Name: "c", Step: &cleaning{stub{name: "c", log: &log}}},
	})
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "b broke") {
		t.Fatalf("err = %v, want the timeout of b", err)
	}
	want := []string{"check a", "check b", "check c", "run a ", "run b ", "cleanup b", "cleanup a"}
	if !slices.Equal(log, want) {
		t.Errorf("log = %q\nwant  %q", log, want)
	}
}

func TestInterruptPopsCleanups(t *testing.T) {
	var log []string
	r := newRun(&log)
	ctx, cancel := context.WithCancel(context.Background())
	r.Ctx = ctx
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "a", Step: &cleaning{stub{name: "a", log: &log, cancel: cancel}}},
		{Name: "b", Step: &stub{name: "b", log: &log}},
	})
	if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(log, "run b ") || !slices.Contains(log, "cleanup a") {
		t.Errorf("log = %q", log)
	}
}

func TestUndoRunsTheCleanupOnce(t *testing.T) {
	var log []string
	r := newRun(&log)
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "a", ID: "first", Step: &cleaning{stub{name: "a", log: &log}}},
		{Name: "b", Step: &cleaning{stub{name: "b", log: &log}}},
		{Name: "undo", Step: pipeline.Undo{ID: "first"}},
		{Name: "undo", Step: pipeline.Undo{ID: "first"}},
		{Name: "c", Step: &stub{name: "c", log: &log}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"check a", "check b", "check c", "run a ", "run b ", "cleanup a", "run c ", "cleanup b"}
	if !slices.Equal(log, want) {
		t.Errorf("log = %q\nwant  %q", log, want)
	}
}

func TestFamilySkipsStep(t *testing.T) {
	var log []string
	r := newRun(&log)
	r.Family = "ubuntu"
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "a", Family: "omarchy", Step: &cleaning{stub{name: "a", log: &log}}},
		{Name: "b", Family: "ubuntu", Step: &stub{name: "b", log: &log}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"check a", "check b", "run b "}
	if !slices.Equal(log, want) {
		t.Errorf("log = %q\nwant  %q", log, want)
	}
}
