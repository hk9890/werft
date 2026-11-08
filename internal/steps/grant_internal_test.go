package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGrantSudoWritesBothLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grant")
	if out, err := exec.Command("sh", "-c", grantSudo, "sh", "tester", file).CombinedOutput(); err != nil {
		t.Fatalf("grant script: %v\n%s", err, out)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "tester ALL=(ALL) NOPASSWD:ALL\nDefaults:tester verifypw=any\n"
	if string(got) != want {
		t.Errorf("grant = %q, want %q", got, want)
	}
}
