package libvirt

import (
	"bytes"
	"errors"
	"fmt"
	"syscall"
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

func TestMetadataErrTreatsATransientDomainAsNoMetadata(t *testing.T) {
	transient := golibvirt.Error{Code: uint32(golibvirt.ErrOperationInvalid), Message: "transient domains do not have any persistent config"}
	if err := metadataErr(true, transient, "uri", "vm1"); !failure.Is(err, failure.NotFound) {
		t.Errorf("metadataErr(transient) = %v (%s), want not_found", err, failure.KindOf(err))
	}
	if err := metadataErr(true, golibvirt.Error{Code: uint32(golibvirt.ErrInternalError), Message: "x"}, "uri", "vm1"); !failure.Is(err, failure.Failed) {
		t.Errorf("metadataErr(internal error) = %v (%s), want failed", err, failure.KindOf(err))
	}
	if metadataErr(true, nil, "uri", "vm1") != nil {
		t.Error("metadataErr(nil) is not nil")
	}
}

func TestWrapKeepsLibvirtMessage(t *testing.T) {
	err := wrap(true, golibvirt.Error{Code: uint32(golibvirt.ErrNoDomain), Message: "Domain not found"}, "domain %s", "vm1")
	if err.Error() != "domain vm1: Domain not found" || !failure.Is(err, failure.NotFound) {
		t.Errorf("wrap = %v (%s)", err, failure.KindOf(err))
	}
	if wrap(true, nil, "x") != nil {
		t.Error("wrap(nil) is not nil")
	}
}

func TestWrapReportsALostConnection(t *testing.T) {
	err := wrap(false, syscall.EINVAL, "domain %s", "vm1")
	want := "domain vm1: lost the connection to libvirt (libvirtd restarted?); the domain may still run"
	if err.Error() != want || !failure.Is(err, failure.Failed) || !errors.Is(err, syscall.EINVAL) {
		t.Errorf("wrap = %v (%s), want %q (failed) around EINVAL", err, failure.KindOf(err), want)
	}
	if wrap(false, nil, "x") != nil {
		t.Error("wrap(nil) on a closed connection is not nil")
	}
	transient := golibvirt.Error{Code: uint32(golibvirt.ErrOperationInvalid), Message: "transient domains do not have any persistent config"}
	if err := metadataErr(false, transient, "uri", "vm1"); !errors.As(err, new(lostConnection)) {
		t.Errorf("metadataErr on a closed connection = %v, want the lost connection", err)
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
