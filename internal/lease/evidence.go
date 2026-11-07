package lease

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
// directory as <YYYYmmdd-HHMMSS>-<domain><ext>, the time in UTC, then
// removes all but the KeepEvidence newest files there. The time comes
// first, so the names sort by age. The prune never removes the new file.
func saveScreen(e Env, domain string) (string, error) {
	var buf bytes.Buffer
	mime, err := e.H.Screenshot(domain, &buf)
	if err != nil {
		return "", err
	}
	ext := screenExt[mime]
	if ext == "" {
		ext = ".bin"
	}
	dir := e.Dir.Evidence()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, e.Now().UTC().Format("20060102-150405")+"-"+domain+ext)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(dir)
	older := slices.DeleteFunc(entries, func(f os.DirEntry) bool { return f.Name() == filepath.Base(path) })
	for _, old := range older[:max(0, len(older)-(KeepEvidence-1))] {
		_ = os.Remove(filepath.Join(dir, old.Name()))
	}
	return path, nil
}

// screenExt is the file extension of each image type a screenshot gives.
// Any other type gets .bin, so no viewer takes it for an image.
var screenExt = map[string]string{
	"image/png":               ".png",
	"image/x-portable-pixmap": ".ppm",
}
