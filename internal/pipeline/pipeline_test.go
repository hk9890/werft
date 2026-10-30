package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virttest"
)

type stub struct {
	name      string
	log       *[]string
	failRun   bool
	failCheck bool
	ask       string
	declare   string
	cancel    context.CancelFunc
}

func (s *stub) Check(r *pipeline.Run) error {
	*s.log = append(*s.log, "check "+s.name)
	if s.ask != "" {
		r.Ask(s.ask)
	}
	if s.declare != "" {
		r.Declare(s.declare, secret.Spec{Prompt: s.declare})
	}
	if s.failCheck {
		return failure.New(failure.Conflict, "%s is busy", s.name)
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

type cleaning struct {
	stub
	// failCleanups is how many cleanups fail before one succeeds.
	failCleanups int
}

func (c *cleaning) Cleanup(*pipeline.Run) error {
	*c.log = append(*c.log, "cleanup "+c.name)
	if c.failCleanups > 0 {
		c.failCleanups--
		return errors.New("ssh timed out")
	}
	return nil
}

type inspector struct{ cleaning }

func (*inspector) KeepOnFailure() {}

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
		{Name: "b", Step: &cleaning{stub: stub{name: "b", log: &log}}},
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
		{Name: "a", Step: &cleaning{stub: stub{name: "a", log: &log}}},
		{Name: "b", Step: &cleaning{stub: stub{name: "b", log: &log, failRun: true}}},
		{Name: "c", Step: &cleaning{stub: stub{name: "c", log: &log}}},
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
		{Name: "a", Step: &cleaning{stub: stub{name: "a", log: &log, cancel: cancel}}},
		{Name: "b", Step: &stub{name: "b", log: &log}},
	})
	if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(log, "run b ") || !slices.Contains(log, "cleanup a") {
		t.Errorf("log = %q", log)
	}
}

func TestInterruptBeforeTheFirstStepRunsNoStep(t *testing.T) {
	var log []string
	r := newRun(&log)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Ctx = ctx
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "reset", Step: &stub{name: "reset", log: &log}},
	})
	if err == nil || !strings.Contains(err.Error(), "interrupted before reset") {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(log, "run reset ") {
		t.Errorf("log = %q", log)
	}
}

func TestFailedUndoIsTriedAgainAtTheEnd(t *testing.T) {
	var log []string
	r := newRun(&log)
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "a", Step: &cleaning{stub: stub{name: "a", log: &log}, failCleanups: 1}},
		{Name: "undo", Step: pipeline.Undo{ID: "a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "ssh timed out") {
		t.Fatalf("err = %v", err)
	}
	want := []string{"check a", "run a ", "cleanup a", "cleanup a"}
	if !slices.Equal(log, want) {
		t.Errorf("log = %q\nwant  %q", log, want)
	}
}

func TestInspectorStaysAfterAFailureOnly(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var log []string
		r := newRun(&log)
		_ = pipeline.Execute(r, []pipeline.Entry{
			{Name: "viewer", Step: &inspector{cleaning{stub: stub{name: "viewer", log: &log}}}},
			{Name: "b", Step: &stub{name: "b", log: &log, failRun: fail}},
		})
		if closed := slices.Contains(log, "cleanup viewer"); closed == fail {
			t.Errorf("failed run %v: log = %q", fail, log)
		}
	}
}

func TestUndoRunsTheCleanupOnce(t *testing.T) {
	var log []string
	r := newRun(&log)
	err := pipeline.Execute(r, []pipeline.Entry{
		{Name: "a", ID: "first", Step: &cleaning{stub: stub{name: "a", log: &log}}},
		{Name: "b", Step: &cleaning{stub: stub{name: "b", log: &log}}},
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
		{Name: "a", Family: "omarchy", Step: &cleaning{stub: stub{name: "a", log: &log}}},
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

func TestEveryCheckComesBeforeTheFirstQuestion(t *testing.T) {
	var log []string
	r := newRun(&log)
	entries := []pipeline.Entry{
		{Name: "a", Step: &stub{name: "a", log: &log, ask: "reset?", declare: "s1"}},
		{Name: "b", Step: &stub{name: "b", log: &log, ask: "replace?"}},
	}
	if err := pipeline.Execute(r, entries); err != nil {
		t.Fatal(err)
	}
	want := []string{"check a", "check b", "confirm reset?", "confirm replace?", "secret s1", "run a v-s1", "run b "}
	if !slices.Equal(log, want) {
		t.Errorf("log = %q\nwant  %q", log, want)
	}

	log = nil
	r = newRun(&log)
	entries[1].Step = &stub{name: "b", log: &log, failCheck: true}
	if err := pipeline.Execute(r, entries); !failure.Is(err, failure.Conflict) {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"check a", "check b"}; !slices.Equal(log, want) {
		t.Errorf("a failed check still asked: log = %q", log)
	}

	log = nil
	r = newRun(&log)
	r.Yes = true
	entries[1].Step = &stub{name: "b", log: &log}
	if err := pipeline.Execute(r, entries); err != nil || slices.Contains(log, "confirm reset?") {
		t.Errorf("--yes still asked: %v, log = %q", err, log)
	}
}

// SIGINT ends a wait at once, not after its pause.
func TestInterruptEndsAPause(t *testing.T) {
	var log []string
	r := newRun(&log)
	ctx, cancel := context.WithCancel(context.Background())
	r.Ctx = ctx
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := r.Poll(time.Hour, time.Hour, "the guest", func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "interrupted while waiting: the guest") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the wait took %v after the interrupt", d)
	}
}

func TestPollLoginPausesGrow(t *testing.T) {
	var log []string
	r := newRun(&log)
	var pauses []time.Duration
	r.Sleep = func(d time.Duration) { pauses = append(pauses, d) }
	err := r.PollLogin(20*time.Second, "no SSH", func() bool { return false })
	if !failure.Is(err, failure.Timeout) || err.Error() != "no SSH within 20s" {
		t.Fatalf("err = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 5 * time.Second}
	if !slices.Equal(pauses, want) {
		t.Errorf("pauses = %v, want %v", pauses, want)
	}
}
