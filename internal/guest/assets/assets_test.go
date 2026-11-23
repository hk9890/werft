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

const sealKey = "ssh-ed25519 AAAAseal seal"

// runSealTCP runs seal-tcp.sh with its fixed paths moved under a temp dir,
// and a systemctl stub that reports version.
func runSealTCP(t *testing.T, version, keys string) (dir string, out []byte, err error) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, filepath.Join(dir, "root/.ssh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\necho 'systemd " + version + " (" + version + ".4-1)'\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"getent": "#!/bin/sh\necho \"tester:x:1000:1000::${STUB_HOME:-/home/tester}:/bin/sh\"\n",
		"id":     "#!/bin/sh\necho \"${STUB_GROUP:-tester}\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "hosts"), []byte("127.0.0.1\tlocalhost\n127.0.1.1\tbuildvm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if keys != "" {
		if err := os.WriteFile(filepath.Join(dir, "root/.ssh/authorized_keys"), []byte(keys), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.NewReplacer(
		"/root/.ssh/authorized_keys", filepath.Join(dir, "root/.ssh/authorized_keys"),
		"/etc/hosts", filepath.Join(dir, "hosts"),
	).Replace(assets.SealTCP)
	c := exec.Command("sh", "-c", script, "sh", "tester", "", sealKey)
	c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err = c.Output()
	return dir, out, err
}

func TestSealTCPRemovesOnlyTheRunKey(t *testing.T) {
	const other = "ssh-ed25519 AAAAother user"
	tests := map[string]struct{ keys, want string }{
		"key among others":         {other + "\n\n" + sealKey + "\n", other + "\n\n"},
		"key alone":                {"\n" + sealKey + "\n", "\n"},
		"longer line stays":        {sealKey + " more\n" + sealKey + "\n", sealKey + " more\n"},
		"key twice, after reboots": {sealKey + "\n" + other + "\n" + sealKey + "\n", other + "\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir, out, err := runSealTCP(t, "255", tt.keys)
			if err != nil {
				t.Fatalf("seal-tcp.sh: %v", err)
			}
			if len(out) != 0 {
				t.Errorf("stdout = %q, want nothing: seal reads the guest facts there", out)
			}
			got, err := os.ReadFile(filepath.Join(dir, "root/.ssh/authorized_keys"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("authorized_keys = %q, want %q", got, tt.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "root/.ssh/authorized_keys.werft")); !os.IsNotExist(err) {
				t.Errorf("the temporary file is left: %v", err)
			}
			hosts, err := os.ReadFile(filepath.Join(dir, "hosts"))
			if err != nil || string(hosts) != "127.0.0.1\tlocalhost\n" {
				t.Errorf("hosts = %q, %v; the build VM name must go", hosts, err)
			}
		})
	}
}

func TestSealTCPRefusesAUserALeaseCannotReach(t *testing.T) {
	tests := map[string]struct{ home, group string }{
		"another home":  {"/var/home/tester", "tester"},
		"another group": {"/home/tester", "users"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("STUB_HOME", tt.home)
			t.Setenv("STUB_GROUP", tt.group)
			dir, _, err := runSealTCP(t, "255", sealKey+"\n")
			if err == nil {
				t.Fatal("seal-tcp.sh passed")
			}
			if got, err := os.ReadFile(filepath.Join(dir, "hosts")); err != nil || !strings.Contains(string(got), "127.0.1.1") {
				t.Errorf("hosts = %q, %v; a refused run must leave it", got, err)
			}
		})
	}
}

func TestSealTCPFails(t *testing.T) {
	tests := map[string]struct{ version, keys string }{
		"grep error: no key file": {"255", ""},
		"another systemd":         {"256", sealKey + "\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runSealTCP(t, tt.version, tt.keys); err == nil {
				t.Fatal("seal-tcp.sh passed")
			}
		})
	}
}

// TestSealLinksTheDBusMachineID runs seal.sh under a temp root. Every tool
// that acts on the system is a stub, and every /etc path moves under the root.
func TestSealLinksTheDBusMachineID(t *testing.T) {
	const id = "var/lib/dbus/machine-id"
	tests := map[string]struct {
		prepare  func(t *testing.T, dir string)
		wantLink bool
	}{
		"regular file becomes a link": {func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, id), "old\n")
		}, true},
		"link stays": {func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, id)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "etc/machine-id"), filepath.Join(dir, id)); err != nil {
				t.Fatal(err)
			}
		}, true},
		"directory without the file": {func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, id)), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		"no directory": {func(*testing.T, string) {}, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, d := range []string{"bin", "etc/ssh", "etc/systemd/system", "etc/sudoers.d", "home/tester"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, filepath.Join(dir, "etc/machine-id"), "build\n")
			for tool, body := range map[string]string{
				"getent":      "echo 'tester:x:1000:1000::" + filepath.Join(dir, "home/tester") + ":/bin/sh'",
				"systemctl":   "echo 'systemd 259 (259.1-1)'",
				"visudo":      "exit 0",
				"systemd-run": "exit 0",
				"sync":        "exit 0",
			} {
				if err := os.WriteFile(filepath.Join(dir, "bin", tool), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tt.prepare(t, dir)
			script := strings.NewReplacer(
				"/var/lib/dbus", filepath.Join(dir, "var/lib/dbus"),
				"/etc/", filepath.Join(dir, "etc")+"/",
			).Replace(assets.Seal)
			c := exec.Command("sh", "-c", script, "sh", "tester", "")
			c.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("seal.sh: %v\n%s", err, out)
			}
			link, err := os.Readlink(filepath.Join(dir, id))
			if tt.wantLink && (err != nil || link != filepath.Join(dir, "etc/machine-id")) {
				t.Errorf("D-Bus machine-id link = %q, %v", link, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, id)); !tt.wantLink && !os.IsNotExist(err) {
				t.Errorf("seal.sh made a D-Bus machine-id: %v", err)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
