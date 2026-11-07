package lease_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/lease"
	"github.com/hk9890/werft/internal/virttest"
)

// failBoot starts a lease whose SSH wait times out while its domain shows a
// screen. It returns the evidence directory and the error of the start.
// prepare, when set, can change the environment before the start.
func failBoot(t *testing.T, f *virttest.Fake, prepare func(e *lease.Env)) (string, error) {
	t.Helper()
	defer lease.SetNewID("abc123")()
	e := env(t, f, &shell{up: false})
	e.Sleep = func(time.Duration) { f.SetScreenshot("werft-vm-plain-abc123", "image/png", []byte("\x89PNG screen")) }
	if prepare != nil {
		prepare(&e)
	}
	_, err := lease.Start(e, record(), "agent")
	return e.Dir.Evidence(), err
}

// TestStartSavesTheScreenOfAFailedBoot: a lease whose SSH wait times out
// saves its last screen before the release, and the error names the file.
func TestStartSavesTheScreenOfAFailedBoot(t *testing.T) {
	f := world(t)
	dir, err := failBoot(t, f, nil)
	if !failure.Is(err, failure.Timeout) {
		t.Fatalf("Start = %v, want kind timeout", err)
	}
	path := filepath.Join(dir, "20261103-100000-werft-vm-plain-abc123.png")
	if !strings.Contains(err.Error(), "its last screen is in "+path) {
		t.Errorf("the error does not name %s: %v", path, err)
	}
	if data, rerr := os.ReadFile(path); rerr != nil || string(data) != "\x89PNG screen" {
		t.Errorf("evidence file = %q, %v", data, rerr)
	}
	if domains, _ := f.Domains(); len(domains) != 0 {
		t.Errorf("domains left: %v", domains)
	}
}

// TestStartKeepsTheNewestEvidence: the evidence directory keeps the
// KeepEvidence newest files and removes the older ones.
func TestStartKeepsTheNewestEvidence(t *testing.T) {
	f := world(t)
	var old []string
	dir, _ := failBoot(t, f, func(e *lease.Env) {
		dir := e.Dir.Evidence()
		must(t, os.MkdirAll(dir, 0o700))
		for i := range lease.KeepEvidence {
			name := fmt.Sprintf("20260101-0000%02d-werft-vm-plain-old%03d.png", i, i)
			old = append(old, name)
			must(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
		}
	})
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != lease.KeepEvidence {
		t.Fatalf("%d evidence files, want %d: %v", len(entries), lease.KeepEvidence, entries)
	}
	if _, err := os.Stat(filepath.Join(dir, old[0])); !os.IsNotExist(err) {
		t.Errorf("the oldest file %s is left: %v", old[0], err)
	}
	if _, err := os.Stat(filepath.Join(dir, old[1])); err != nil {
		t.Errorf("the second oldest file %s is gone: %v", old[1], err)
	}
	if last := entries[len(entries)-1].Name(); last != "20261103-100000-werft-vm-plain-abc123.png" {
		t.Errorf("newest file = %s", last)
	}
}

// TestStartSavesNoScreenWithoutABoot: a domain that never started has no
// screen: the error names none, and no evidence directory appears.
func TestStartSavesNoScreenWithoutABoot(t *testing.T) {
	f := world(t)
	f.FailOn("StartDomain", failure.New(failure.Failed, "injected"))
	dir, err := failBoot(t, f, nil)
	if err == nil || strings.Contains(err.Error(), "screen") {
		t.Errorf("Start = %v, want an error without a screen", err)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("evidence directory exists: %v", serr)
	}
}

// TestStartNamesAScreenItCouldNotSave: a failed screenshot keeps the kind
// of the cause, and the error says that no screen was saved.
func TestStartNamesAScreenItCouldNotSave(t *testing.T) {
	f := world(t)
	f.FailOn("Screenshot", failure.New(failure.Failed, "injected screenshot"))
	_, err := failBoot(t, f, nil)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "no last screen saved: injected screenshot") {
		t.Errorf("Start = %v, want kind timeout and the screenshot failure", err)
	}
}

// TestStartKeepsTheNewScreen: files whose names sort after the new one, as
// after a clock step backward, do not push the new screen out.
func TestStartKeepsTheNewScreen(t *testing.T) {
	f := world(t)
	dir, err := failBoot(t, f, func(e *lease.Env) {
		dir := e.Dir.Evidence()
		must(t, os.MkdirAll(dir, 0o700))
		for i := range lease.KeepEvidence {
			must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("20270101-0000%02d-werft-vm-plain-new%03d.png", i, i)), nil, 0o600))
		}
	})
	path := filepath.Join(dir, "20261103-100000-werft-vm-plain-abc123.png")
	if _, serr := os.Stat(path); serr != nil || !strings.Contains(err.Error(), path) {
		t.Errorf("the new screen %s is gone or not named: %v; %v", path, serr, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != lease.KeepEvidence {
		t.Errorf("%d evidence files, want %d", len(entries), lease.KeepEvidence)
	}
}

// TestStartNamesTheScreenInUTC: the time in the file name is UTC, so the
// names sort by age across a change of the local clock.
func TestStartNamesTheScreenInUTC(t *testing.T) {
	f := world(t)
	dir, _ := failBoot(t, f, func(e *lease.Env) {
		e.Now = func() time.Time { return time.Date(2026, 11, 3, 11, 0, 0, 0, time.FixedZone("CET", 3600)) }
	})
	if _, err := os.Stat(filepath.Join(dir, "20261103-100000-werft-vm-plain-abc123.png")); err != nil {
		t.Errorf("no screen with a UTC name: %v", err)
	}
}

// TestStartSavesAnUnknownImageAsBin: a screen of an unknown image type gets
// .bin, so no viewer takes it for a PNG.
func TestStartSavesAnUnknownImageAsBin(t *testing.T) {
	f := world(t)
	dir, _ := failBoot(t, f, func(e *lease.Env) {
		e.Sleep = func(time.Duration) {
			f.SetScreenshot("werft-vm-plain-abc123", "application/octet-stream", []byte("raw"))
		}
	})
	if _, err := os.Stat(filepath.Join(dir, "20261103-100000-werft-vm-plain-abc123.bin")); err != nil {
		t.Errorf("no .bin screen: %v", err)
	}
}
