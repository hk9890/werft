package lease

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// KeepEvidence is how many screens of failed boots the evidence directory
// keeps: the newest.
const KeepEvidence = 10

// evidence saves the last screen of a lease that did not boot, before the
// release destroys it, and is the note the error gets: where the screen is,
// or why it is not there.
func evidence(e Env, domain string) string {
	path, err := saveScreen(e, domain)
	if err != nil {
		return fmt.Sprintf("; no last screen saved: %v", err)
	}
	return "; its last screen is in " + path
}

// saveScreen writes the screen of the running domain to the evidence
// directory as <YYYYmmdd-HHMMSS>-<domain>.png (.ppm when QEMU gives PPM),
// then removes all but the KeepEvidence newest files there. The time comes
// first, so the names sort by age.
func saveScreen(e Env, domain string) (string, error) {
	var buf bytes.Buffer
	mime, err := e.H.Screenshot(domain, &buf)
	if err != nil {
		return "", err
	}
	ext := ".png"
	if mime == "image/x-portable-pixmap" {
		ext = ".ppm"
	}
	dir := e.Dir.Evidence()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, e.Now().Format("20060102-150405")+"-"+domain+ext)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(dir)
	for _, old := range entries[:max(0, len(entries)-KeepEvidence)] {
		_ = os.Remove(filepath.Join(dir, old.Name()))
	}
	return path, nil
}
