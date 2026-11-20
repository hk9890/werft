package image

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hk9890/werft/internal/failure"
)

// TestWritesRefuseUnknownKeys: images.toml with a key that a newer werft
// wrote loads, but add and drop leave it as it is.
func TestWritesRefuseUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.toml")
	content := []byte("removed = [\"vm-plain-20261031\"]\n\n[[image]]\nname = \"vm-plain-20261101\"\n" +
		"disk_password_keyring = { service = \"werft\" }\nnewer_field = \"x\"\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := Load(path)
	if err != nil || len(records) != 1 || records[0].Name != "vm-plain-20261101" {
		t.Fatalf("Load = %+v, %v", records, err)
	}
	for name, write := range map[string]func() error{
		"add":  func() error { return add(path, Record{Name: "vm-plain-20261102"}) },
		"drop": func() error { return drop(path, []string{"vm-plain-20261101"}) },
	} {
		err := write()
		if !failure.Is(err, failure.Unsupported) || !strings.Contains(err.Error(), "image.newer_field") ||
			!strings.Contains(err.Error(), "a newer werft wrote it; update werft") {
			t.Errorf("%s = %v, want unsupported naming image.newer_field", name, err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, content) {
			t.Errorf("%s changed images.toml:\n%s", name, got)
		}
	}
}

// TestWritableNamesEachUnknownKeyOnce: two records with the same unknown
// key name it once.
func TestWritableNamesEachUnknownKeyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.toml")
	content := "[[image]]\nname = \"vm-plain-20261101\"\nnewer_field = \"x\"\n\n" +
		"[[image]]\nname = \"vm-plain-20261102\"\nnewer_field = \"y\"\nolder_field = \"z\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Writable(path)
	if !failure.Is(err, failure.Unsupported) || strings.Count(err.Error(), "image.newer_field") != 1 ||
		!strings.Contains(err.Error(), "image.newer_field, image.older_field") {
		t.Errorf("Writable = %v, want image.newer_field once, then image.older_field", err)
	}
}
