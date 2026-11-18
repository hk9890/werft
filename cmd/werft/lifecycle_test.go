package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/secret"
	"github.com/hk9890/werft/internal/virt"
	"github.com/hk9890/werft/internal/virttest"
)

// lifecycleVM has its disk in the default pool and its NVRAM in nvramDir.
func lifecycleVM(nvramDir string) string {
	return `<domain type='kvm'><name>vm1</name><os><type>hvm</type>
<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
<nvram format='raw'>` + nvramDir + `/vm1_VARS.fd</nvram></os><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/img/vm1.qcow2'/><target dev='vda'/></disk>
<interface type='network'><mac address='52:54:00:00:00:01'/><source network='default'/></interface>
</devices></domain>`
}

type lifecycleHost struct {
	fake     *virttest.Fake
	nvramDir string
	viewed   []string
	viewErr  error
	// lease is the DHCP lease the guest takes during the first wait.
	lease *virt.DHCPLease
	// secrets is the keyring and pass of the host; screen shows a console
	// with a prompt, so start can type a disk password.
	secrets secret.Store
	screen  bool
	// tty makes stdin a terminal whose answer is answer, else answerErr.
	tty       bool
	answer    string
	answerErr error
	// keysAtView is the number of keys sent before each viewer opened.
	keysAtView []int
}

func newLifecycleHost(t *testing.T) *lifecycleHost {
	t.Helper()
	f := virttest.New()
	if err := virt.EnsurePool(f, "default", "/img"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateVolume("default", "<volume><name>vm1.qcow2</name><capacity unit='G'>20</capacity></volume>"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := f.DefineDomain(lifecycleVM(dir)); err != nil {
		t.Fatal(err)
	}
	return &lifecycleHost{fake: f, nvramDir: dir, secrets: keyring("")}
}

func (lh *lifecycleHost) setLUKS(t *testing.T) {
	t.Helper()
	if err := lh.fake.SetDomainDescription("vm1", "vm: the disk asks for the guest password at boot"); err != nil {
		t.Fatal(err)
	}
}

func (lh *lifecycleHost) run(args ...string) result {
	var out, errOut bytes.Buffer
	c := &cli{
		stdin: strings.NewReader(""), stdout: &out, stderr: &errOut,
		dial: func() (virt.Hypervisor, error) {
			if lh.screen {
				return promptScreen{lh.fake}, nil
			}
			return lh.fake, nil
		},
		secrets: lh.secrets,
		sleep: func(time.Duration) {
			if lh.lease != nil {
				lh.fake.AddDHCPLease("default", *lh.lease)
				lh.lease = nil
			}
		},
		tty:        lh.tty,
		readSecret: func() (string, error) { return lh.answer, lh.answerErr },
		openViewer: func(domain string) error {
			lh.keysAtView = append(lh.keysAtView, len(lh.fake.SentKeys(domain)))
			lh.viewed = append(lh.viewed, domain)
			return lh.viewErr
		},
	}
	code := c.run(args)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func TestListText(t *testing.T) {
	lh := newLifecycleHost(t)
	r := lh.run("list")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 2 {
		t.Fatalf("list = %+v", r)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "NAME STATE USED SIZE RESET LUKS ADDRESS" {
		t.Errorf("header = %q", lines[0])
	}
	if got := strings.Join(strings.Fields(lines[1]), " "); got != "vm1 shut off 0 B 20.00 GiB 0 - -" {
		t.Errorf("row = %q", got)
	}

	if err := os.WriteFile(filepath.Join(lh.nvramDir, "vm1_VARS.fd.clean.bak"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lh.setLUKS(t)
	if err := lh.fake.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	lh.fake.AddDHCPLease("default", virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.5"})
	r = lh.run("list")
	lines = strings.Split(strings.TrimSpace(r.stdout), "\n")
	if got := strings.Join(strings.Fields(lines[len(lines)-1]), " "); r.code != 0 || got != "vm1 running 0 B 20.00 GiB 1 yes 192.168.122.5" {
		t.Errorf("row of the running VM = %q, %+v", got, r)
	}
}

func TestListJSON(t *testing.T) {
	lh := newLifecycleHost(t)
	outside := strings.NewReplacer("vm1", "vm2", "/img/", "/elsewhere/", "52:54:00:00:00:01", "52:54:00:00:00:02").Replace(lifecycleVM(lh.nvramDir))
	if _, err := lh.fake.DefineDomain(outside); err != nil {
		t.Fatal(err)
	}
	r := lh.run("--json", "list")
	var doc []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 0 || len(doc) != 2 {
		t.Fatalf("list --json = %+v", r)
	}
	want := map[string]any{"name": "vm1", "state": "shut off", "used_bytes": float64(0), "size_bytes": float64(20 << 30), "reset_points": float64(0), "luks": false, "address": "", "lease": false}
	if !reflect.DeepEqual(doc[0], want) {
		t.Errorf("entry = %v, want %v", doc[0], want)
	}
	if v, ok := doc[1]["used_bytes"]; !ok || v != nil || doc[1]["size_bytes"] != nil {
		t.Errorf("entry of a disk in no pool = %v, want null sizes", doc[1])
	}

	empty := &lifecycleHost{fake: virttest.New()}
	if r := empty.run("--json", "list"); r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("list --json without domains = %+v", r)
	}
}

func TestStartWaitsAndPrintsTheAddress(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.lease = &virt.DHCPLease{MAC: "52:54:00:00:00:01", IP: "192.168.122.5"}
	r := lh.run("start", "vm1")
	if r.code != 0 || r.stdout != "address: 192.168.122.5   ssh 192.168.122.5\n" || !strings.Contains(r.stderr, "vm1 started (headless, no window)") {
		t.Errorf("start = %+v", r)
	}
	if len(lh.viewed) != 0 {
		t.Error("start without --view opened a viewer")
	}

	r = lh.run("--json", "start", "--view", "vm1")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 0 || doc["already_running"] != true || doc["address"] != "192.168.122.5" || !strings.Contains(r.stderr, "already running") {
		t.Errorf("start --view of a running VM = %+v", r)
	}
	if len(lh.viewed) != 1 || lh.viewed[0] != "vm1" || !strings.Contains(r.stderr, "Shift+F11 fullscreen, Shift+F12 release cursor") {
		t.Errorf("viewer = %q, stderr %q", lh.viewed, r.stderr)
	}
}

func TestStartWithoutAddressOrViewerStillSucceeds(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.viewErr = errors.New("no display here")
	r := lh.run("start", "--view", "vm1")
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "No address yet") || !strings.Contains(r.stderr, "no viewer window: no display here") {
		t.Errorf("start = %+v", r)
	}

	if err := lh.fake.DestroyDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	r = lh.run("--json", "start", "vm1")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 0 || doc["address"] != "" || !strings.Contains(r.stderr, "No address yet") {
		t.Errorf("start --json without a lease = %+v", r)
	}
}

// TestStartTellsAboutTheDiskPassword: without a recipe source, start asks
// nothing, also at a terminal, and leaves the password to the person.
func TestStartTellsAboutTheDiskPassword(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	lh.tty, lh.answerErr = true, errors.New("test: start asked")
	r := lh.run("start", "vm1")
	if r.code != 0 || !strings.Contains(r.stderr, "werft does not type it. Type it in the viewer: werft start --view vm1") || len(lh.fake.SentKeys("vm1")) != 0 {
		t.Errorf("start of a LUKS VM = %+v", r)
	}

	if r := lh.run("start", "vm1"); r.code != 0 || strings.Contains(r.stderr, "password") {
		t.Errorf("start of a running LUKS VM = %+v; it is unlocked already", r)
	}

	if err := lh.fake.DestroyDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	r = lh.run("start", "--view", "vm1")
	if r.code != 0 || !strings.Contains(r.stderr, "Type it in the open viewer window") || strings.Contains(r.stderr, "werft start --view") {
		t.Errorf("start --view of a LUKS VM = %+v", r)
	}
}

// promptScreen shows a console that is not blank: the disk password prompt.
type promptScreen struct{ *virttest.Fake }

func (s promptScreen) Screenshot(domain string, w io.Writer) (string, error) {
	if _, err := s.Fake.Screenshot(domain, io.Discard); err != nil {
		return "", err
	}
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.White)
	return "image/png", png.Encode(w, img)
}

// keyring is a keyring with one item, service=werft username=tester, and an
// empty pass. An empty keyring gives nothing, so no test reads the host's.
type keyring string

func (k keyring) Lookup(attributes map[string]string) (string, error) {
	if k != "" && secret.Attributes(attributes) == "service=werft username=tester" {
		return string(k), nil
	}
	return "", failure.New(failure.SecretUnavailable, "no item")
}

func (keyring) Show(entry string) (string, error) {
	return "", failure.New(failure.SecretUnavailable, "pass show %s failed", entry)
}

// writeRecipe makes the recipe of every build VM name its guest password
// in the keyring.
func writeRecipe(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "werft")
	recipe := filepath.Join(dir, "recipe.toml")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipe, []byte("[guest_password]\nkeyring = { service = \"werft\", username = \"tester\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("recipe = \""+recipe+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStartTypesTheDiskPasswordFromTheKeyring(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.secrets, lh.screen = keyring("kr"), true
	r := lh.run("start", "vm1")
	// k, r, Enter: once.
	if keys := lh.fake.SentKeys("vm1"); r.code != 0 || len(keys) != 3 || keys[0][0] != 37 || keys[2][0] != 28 {
		t.Errorf("start of a LUKS VM = %+v, keys %v; want the password and Enter once", r, keys)
	}
	if !strings.Contains(r.stderr, "comes from the keyring (service=werft username=tester)") || strings.Contains(r.stderr, "werft does not type it") || strings.Contains(r.stderr, "kr") {
		t.Errorf("start of a LUKS VM = %+v", r)
	}

	if r := lh.run("start", "vm1"); r.code != 0 || len(lh.fake.SentKeys("vm1")) != 3 {
		t.Errorf("start of a running LUKS VM = %+v; it is unlocked already", r)
	}
}

func TestStartWithoutTheDiskPasswordLeavesItToThePerson(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	r := lh.run("start", "vm1")
	if r.code != 0 || !strings.Contains(r.stderr, "werft cannot read the disk password of vm1") ||
		!strings.Contains(r.stderr, "Type it in the viewer: werft start --view vm1") || len(lh.fake.SentKeys("vm1")) != 0 {
		t.Errorf("start of a LUKS VM without its password = %+v", r)
	}
}

func TestStartLeavesAnUntypeableDiskPasswordToThePerson(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.secrets, lh.screen = keyring("pä"), true
	r := lh.run("start", "vm1")
	if r.code != 0 || strings.Contains(r.stdout+r.stderr, "pä") || !strings.Contains(r.stderr, "werft does not type it") || len(lh.fake.SentKeys("vm1")) != 0 {
		t.Errorf("start with an untypeable password = %+v, want a start that leaves the password to the person, without the password", r)
	}
}

func TestStartWithAnUnreadableRecipeStillStarts(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	if err := os.Remove(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "werft", "recipe.toml")); err != nil {
		t.Fatal(err)
	}
	if r := lh.run("start", "vm1"); r.code != 0 || !strings.Contains(r.stderr, "werft cannot read the disk password of vm1") {
		t.Errorf("start with a missing recipe = %+v, want a start with a warning", r)
	}
}

// TestStartEndsWhenThePersonRefusesThePrompt: the keyring item of the
// recipe fails, so start asks; an interrupt at the prompt ends start before
// the boot.
func TestStartEndsWhenThePersonRefusesThePrompt(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.tty, lh.answerErr = true, errors.New("interrupted by interrupt")
	if r := lh.run("start", "vm1"); r.code == 0 || !strings.Contains(r.stderr, "cannot give it, so it is asked for") {
		t.Errorf("start after an interrupted prompt = %+v, want a failure", r)
	}
	for _, c := range lh.fake.Calls() {
		if strings.HasPrefix(c, "StartDomain") {
			t.Errorf("start after an interrupted prompt called %s", c)
		}
	}
}

// TestStartTypesTheAnswerWhenTheSourceFails: the keyring item of the recipe
// fails, so start asks, and types the answer under the domain lock.
func TestStartTypesTheAnswerWhenTheSourceFails(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.screen, lh.tty, lh.answer = true, true, "kr"
	release, err := lockDomain("vm1")
	if err != nil {
		t.Fatal(err)
	}
	if r := lh.run("start", "vm1"); r.code == 0 || len(lh.fake.SentKeys("vm1")) != 0 {
		t.Errorf("start while another werft holds the lock = %+v, want conflict", r)
	}
	release()
	r := lh.run("start", "vm1")
	if keys := lh.fake.SentKeys("vm1"); r.code != 0 || len(keys) != 3 || keys[0][0] != 37 || keys[2][0] != 28 || strings.Contains(r.stdout+r.stderr, "kr") {
		t.Errorf("start with an answered prompt = %+v, keys %v; want the answer and Enter once", r, keys)
	}
}

// TestStartBootsOnAnEmptyAnswer: Enter alone at the prompt leaves the
// password to the person at the console, and start boots.
func TestStartBootsOnAnEmptyAnswer(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.screen, lh.tty = true, true
	r := lh.run("start", "vm1")
	if r.code != 0 || !strings.Contains(r.stderr, "vm1 started") || !strings.Contains(r.stderr, "werft does not type it") || len(lh.fake.SentKeys("vm1")) != 0 {
		t.Errorf("start after an empty answer = %+v, want a boot that leaves the password to the person", r)
	}
}

// TestStartOpensTheViewerAfterTyping: a person who sees the prompt in the
// viewer would type the password, and werft would then type it again into
// the next still screen.
func TestStartOpensTheViewerAfterTyping(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.secrets, lh.screen = keyring("kr"), true
	if r := lh.run("start", "--view", "vm1"); r.code != 0 || len(lh.keysAtView) != 1 || lh.keysAtView[0] != 3 {
		t.Errorf("start --view = %+v, keys at view %v; want the viewer after the password and Enter", r, lh.keysAtView)
	}
}

func TestStartTakesTheDomainLockToType(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.secrets, lh.screen = keyring("kr"), true
	release, err := lockDomain("vm1")
	if err != nil {
		t.Fatal(err)
	}
	r := lh.run("start", "vm1")
	release()
	if r.code == 0 || len(lh.fake.SentKeys("vm1")) != 0 {
		t.Errorf("start while another werft holds the lock = %+v, want conflict", r)
	}
}

func TestStartNamesTheConsoleWhenTypingFails(t *testing.T) {
	lh := newLifecycleHost(t)
	lh.setLUKS(t)
	writeRecipe(t)
	lh.secrets, lh.screen = keyring("kr"), true
	lh.fake.FailOn("SendKey", errors.New("test: no keys"))
	r := lh.run("--json", "start", "vm1")
	if r.code == 0 || !strings.Contains(r.stdout, "vm1 runs; look at its console") || !strings.Contains(r.stdout, "virt-viewer") {
		t.Errorf("start whose typing fails = %+v, want an error that names the console", r)
	}
}

func TestStop(t *testing.T) {
	lh := newLifecycleHost(t)
	if r := lh.run("stop", "vm1"); r.code != 0 || r.stdout != "vm1 is already shut off\n" {
		t.Errorf("stop of a shut-off VM = %+v", r)
	}
	if err := lh.fake.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	if r := lh.run("stop", "vm1"); r.code != 0 || r.stdout != "vm1 is shut off\n" || !strings.Contains(r.stderr, "ACPI shutdown sent") {
		t.Errorf("stop = %+v", r)
	}
	if err := lh.fake.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	r := lh.run("--json", "stop", "-f", "vm1")
	var doc map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is no JSON: %q", r.stdout)
	}
	if r.code != 0 || doc["result"] != "forced_off" || doc["state"] != "shut off" {
		t.Errorf("stop -f --json = %+v", doc)
	}
}

func TestLifecycleExitCodes(t *testing.T) {
	lh := newLifecycleHost(t)
	if err := lh.fake.StartDomain("vm1"); err != nil {
		t.Fatal(err)
	}
	lh.fake.IgnoreShutdown("vm1")
	paused := strings.ReplaceAll(lifecycleVM(lh.nvramDir), "vm1", "paused")
	if _, err := lh.fake.DefineDomain(strings.Replace(paused, "52:54:00:00:00:01", "52:54:00:00:00:03", 1)); err != nil {
		t.Fatal(err)
	}
	lh.fake.SetState("paused", virt.StatePaused)
	if r := lh.run("start", "paused"); !strings.Contains(r.stderr, "virsh -c qemu:///system resume paused") {
		t.Errorf("start of a paused VM = %+v", r)
	}
	tests := []struct {
		args []string
		code int
	}{
		{[]string{"stop", "vm1"}, 7},
		{[]string{"stop", "none"}, 3},
		{[]string{"start", "none"}, 3},
		{[]string{"start", "paused"}, 4},
		{[]string{"stop"}, 2},
		{[]string{"start", "a", "b"}, 2},
		{[]string{"list", "extra"}, 2},
		{[]string{"stop", "--bogus", "vm1"}, 2},
	}
	for _, tt := range tests {
		if r := lh.run(tt.args...); r.code != tt.code {
			t.Errorf("%q = %+v, want exit %d", tt.args, r, tt.code)
		}
	}
}
