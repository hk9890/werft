package assets_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/guest/assets"
)

func TestPinentryAnswersGETPIN(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "werft-passphrase"), []byte("pa%ss\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "pinentry-auto")
	if err := os.WriteFile(script, assets.PinentryAuto, 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", script)
	c.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir)
	c.Stdin = strings.NewReader("OPTION ttyname=x\nGETPIN\nBYE\n")
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "OK Pleased to meet you\nOK\nD pa%25ss\nOK\nOK closing connection\n"
	if string(out) != want {
		t.Errorf("pinentry said\n%s\nwant\n%s", out, want)
	}
}
