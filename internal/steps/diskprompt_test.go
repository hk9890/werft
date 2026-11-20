package steps_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/pipeline"
	"github.com/hk9890/werft/internal/steps"
)

func bootOnly() []pipeline.Entry {
	return []pipeline.Entry{{Name: "boot", Step: steps.Boot{}}}
}

// A person typed the disk password in a viewer an earlier run left open, and
// the console shows a desktop that moves: no screen holds still. The SSH
// probe sees the guest up, and werft types nothing.
func TestBootSeesADiskOpenedByHand(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	r := w.newRun(true)
	sleeps := 0
	r.Sleep = func(time.Duration) {
		sleeps++
		img := image.NewRGBA(image.Rect(0, 0, 4, 4))
		img.Set(sleeps%4, 1, color.White)
		var frame bytes.Buffer
		must(t, png.Encode(&frame, img))
		w.fake.SetScreenshot(vm, "image/png", frame.Bytes())
		if sleeps == 3 {
			w.shell.locked = false
		}
	}
	if err := pipeline.Execute(r, bootOnly()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if w.index("^SendKey") >= 0 {
		t.Errorf("keys were typed at an open disk:\n  %s", strings.Join(w.entries(), "\n  "))
	}
	if !strings.Contains(w.out.String(), "The disk of testvm was opened already, by hand; werft typed nothing") {
		t.Errorf("no word of the open disk:\n%s", w.out.String())
	}
	if r.LUKS {
		t.Error("LUKS is set, so a later SSH failure would blame a password werft never typed")
	}
	if r.Addr.Host == "" {
		t.Error("the probe set no address")
	}
}

// A disk that stays locked gets the password at the still prompt, as before:
// the probe does not end the wait.
func TestBootTypesWhenTheProbeFails(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	if _, err := w.run(bootOnly(), true); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if n := len(w.fake.SentKeys(vm)); n != len(password)+1 {
		t.Errorf("%d keys typed, want the password and Enter once", n)
	}
}

func TestPromptTimeoutNamesADiskOpenedByHand(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	var black bytes.Buffer
	must(t, png.Encode(&black, image.NewRGBA(image.Rect(0, 0, 4, 4))))
	w.fake.SetScreenshot(vm, "image/png", black.Bytes())
	_, err := w.run(bootOnly(), true)
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "if the console shows a desktop, the disk was opened by hand; close the old viewer windows and run again") {
		t.Fatalf("err = %v", err)
	}
}

// After werft typed, a guest with no SSH answer has two causes; the hint
// names both.
func TestLockedDiskHintNamesBothCauses(t *testing.T) {
	w := newWorld(t)
	w.omarchy()
	w.shell.fail["^true$"] = true
	_, err := w.run([]pipeline.Entry{{Name: "boot", Step: steps.Boot{}}, {Name: "wait-ssh", Step: steps.WaitSSH{}}}, true)
	if err == nil {
		t.Fatal("wait-ssh succeeded")
	}
	for _, want := range []string{
		"an empty disk password prompt means the password went to a screen before the prompt",
		"a prompt again after a 'wrong' or 'No key available' line means the disk refused it",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestBootWarnsOfAnEarlierViewer(t *testing.T) {
	const warning = "warning: a viewer from an earlier run is open on testvm; type nothing in it while werft waits for the disk prompt"
	for _, earlier := range []bool{true, false} {
		w := newWorld(t)
		w.omarchy()
		w.host.earlierViewer = earlier
		if _, err := w.run(bootOnly(), true); err != nil {
			t.Fatalf("boot: %v", err)
		}
		out := w.out.String()
		if got := strings.Contains(out, warning); got != earlier {
			t.Errorf("earlier viewer %v: warning %v\n%s", earlier, got, out)
		}
		if earlier && strings.Index(out, warning) > strings.Index(out, "Waiting for the disk password prompt") {
			t.Errorf("the warning came after the wait began:\n%s", out)
		}
	}
}
