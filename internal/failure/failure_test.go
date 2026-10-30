package failure_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hk9890/werft/internal/failure"
)

func TestKindOf(t *testing.T) {
	cause := errors.New("socket closed")
	tests := []struct {
		name string
		err  error
		want failure.Kind
	}{
		{"nil", nil, ""},
		{"plain error", cause, failure.Failed},
		{"kinded", failure.New(failure.NotFound, "no domain %q", "x"), failure.NotFound},
		{"wrapped by fmt", fmt.Errorf("reset: %w", failure.New(failure.Conflict, "running")), failure.Conflict},
		{"outer kind wins", failure.Wrap(failure.Timeout, failure.New(failure.NotFound, "x"), "wait"), failure.Timeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failure.KindOf(tt.err); got != tt.want {
				t.Errorf("KindOf = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWrapMessageAndUnwrap(t *testing.T) {
	cause := errors.New("socket closed")
	err := failure.Wrap(failure.Failed, cause, "start %s", "vm1")
	if err.Error() != "start vm1: socket closed" {
		t.Errorf("message = %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("Wrap hides the cause from errors.Is")
	}
	if failure.Wrap(failure.Failed, nil, "x") != nil {
		t.Error("Wrap of nil is not nil")
	}
	if !failure.Is(err, failure.Failed) || failure.Is(nil, failure.Failed) {
		t.Error("Is disagrees with KindOf")
	}
}
