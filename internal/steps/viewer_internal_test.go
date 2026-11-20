package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProc writes a /proc entry for each process: its command line and
// its parent.
func fakeProc(t *testing.T, procs map[string][]string, parents map[string]string) string {
	t.Helper()
	proc := t.TempDir()
	for pid, args := range procs {
		dir := filepath.Join(proc, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tx\nPPid:\t"+parents[pid]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return proc
}

func TestEarlierViewer(t *testing.T) {
	viewer := []string{"/usr/bin/virt-viewer", "-c", "qemu:///system", "--attach", "--wait", "--reconnect", "--", "testvm"}
	tests := []struct {
		name   string
		procs  map[string][]string
		parent string
		want   bool
	}{
		{"viewer of an earlier run", map[string][]string{"200": viewer}, "1", true},
		{"viewer of this run", map[string][]string{"200": viewer}, "100", false},
		{"viewer of another domain", map[string][]string{"200": append(viewer[:len(viewer)-1:len(viewer)-1], "testvm-full")}, "1", false},
		{"no viewer", map[string][]string{"200": {"/usr/bin/vim", "testvm"}}, "1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeProc(t, tt.procs, map[string]string{"200": tt.parent})
			if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := earlierViewer(proc, "testvm", 100); got != tt.want {
				t.Errorf("earlierViewer = %v, want %v", got, tt.want)
			}
		})
	}
}
