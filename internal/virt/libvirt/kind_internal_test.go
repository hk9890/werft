package libvirt

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/hk9890/werft/internal/failure"
)

func TestKindOfMapsLibvirtCodes(t *testing.T) {
	lerr := func(n golibvirt.ErrorNumber) error { return golibvirt.Error{Code: uint32(n), Message: "x"} }
	tests := []struct {
		err  error
		want failure.Kind
	}{
		{lerr(golibvirt.ErrNoDomain), failure.NotFound},
		{lerr(golibvirt.ErrNoStorageVol), failure.NotFound},
		{lerr(golibvirt.ErrNoDomainSnapshot), failure.NotFound},
		{lerr(golibvirt.ErrNoDomainMetadata), failure.NotFound},
		{lerr(golibvirt.ErrOperationInvalid), failure.Conflict},
		{lerr(golibvirt.ErrStorageVolExist), failure.Conflict},
		{lerr(golibvirt.ErrOperationTimeout), failure.Timeout},
		{lerr(golibvirt.ErrArgumentUnsupported), failure.Unsupported},
		{lerr(golibvirt.ErrInternalError), failure.Failed},
		{golibvirt.Error{Code: uint32(golibvirt.ErrOperationFailed), Message: "domain moment s1 already exists"}, failure.Conflict},
		{golibvirt.Error{Code: uint32(golibvirt.ErrOperationFailed), Message: "domain 'vm1' is already defined with uuid 0b0b"}, failure.Conflict},
		{golibvirt.Error{Code: uint32(golibvirt.ErrOperationFailed), Message: "cannot open file"}, failure.Failed},
		{fmt.Errorf("lookup: %w", lerr(golibvirt.ErrNoNetwork)), failure.NotFound},
		{errors.New("connection reset"), failure.Failed},
	}
	for _, tt := range tests {
		if got := kindOf(tt.err); got != tt.want {
			t.Errorf("kindOf(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestWrapKeepsLibvirtMessage(t *testing.T) {
	err := wrap(golibvirt.Error{Code: uint32(golibvirt.ErrNoDomain), Message: "Domain not found"}, "domain %s", "vm1")
	if err.Error() != "domain vm1: Domain not found" || !failure.Is(err, failure.NotFound) {
		t.Errorf("wrap = %v (%s)", err, failure.KindOf(err))
	}
	if wrap(nil, "x") != nil {
		t.Error("wrap(nil) is not nil")
	}
}

func TestImageMIME(t *testing.T) {
	tests := map[string]string{
		"\x89PNG\r\n\x1a\n": "image/png",
		"P6\n640 480\n":     "image/x-portable-pixmap",
		"":                  "application/octet-stream",
	}
	for head, want := range tests {
		if got := imageMIME([]byte(head)); got != want {
			t.Errorf("imageMIME(%q) = %q, want %q", head, got, want)
		}
	}
}

func TestCountingWriterKeepsTheHead(t *testing.T) {
	var out bytes.Buffer
	w := &countingWriter{w: &out}
	_, _ = w.Write([]byte("\x89P"))
	_, _ = w.Write([]byte("NG and the rest"))
	if w.n != 17 || string(w.head) != "\x89PNG and" || out.Len() != 17 {
		t.Errorf("n=%d head=%q out=%d", w.n, w.head, out.Len())
	}
}
