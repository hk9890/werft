package console_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hk9890/werft/internal/console"
	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virttest"
)

func TestChord(t *testing.T) {
	tests := map[rune][]uint32{
		'a': {30}, 'Z': {42, 44}, ' ': {57}, '1': {2}, '!': {42, 2}, '\\': {43}, '|': {42, 43},
		'\'': {40}, '"': {42, 40}, '`': {41}, '~': {42, 41}, '/': {53}, '?': {42, 53},
	}
	for r, want := range tests {
		got, ok := console.Chord(r)
		if !ok || !slices.Equal(got, want) {
			t.Errorf("Chord(%q) = %v, %v; want %v", r, got, ok, want)
		}
	}
	for _, r := range "äé€\t" {
		if _, ok := console.Chord(r); ok {
			t.Errorf("Chord(%q) has a key", r)
		}
	}
}

func TestTypeableHidesTheSecret(t *testing.T) {
	err := console.Typeable("pässwort")
	if err == nil || strings.Contains(err.Error(), "ä") || strings.Contains(err.Error(), "pässwort") {
		t.Errorf("Typeable = %v", err)
	}
}

func screen(t *testing.T, lit bool) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		if i%4 == 3 {
			img.Pix[i] = 0xff
		}
	}
	if lit {
		img.Set(3, 3, color.White)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func running(t *testing.T) *virttest.Fake {
	f := virttest.New()
	if _, err := f.DefineDomain("<domain type='kvm'><name>vm</name></domain>"); err != nil {
		t.Fatal(err)
	}
	if err := f.StartDomain("vm"); err != nil {
		t.Fatal(err)
	}
	return f
}

func prompt(sleeps *int) console.Prompt {
	return console.Prompt{Timeout: 20 * time.Second, Interval: 2 * time.Second, Still: 4, Sleep: func(time.Duration) { *sleeps++ }}
}

func TestWaitForPromptNeedsAStillScreen(t *testing.T) {
	f := running(t)
	f.SetScreenshot("vm", "image/png", screen(t, true))
	sleeps := 0
	if err := console.WaitForPrompt(context.Background(), f, "vm", prompt(&sleeps)); err != nil {
		t.Fatal(err)
	}
	if sleeps != 4 {
		t.Errorf("returned after %d waits, want 4 still screenshots after the first", sleeps)
	}
}

func TestWaitForPromptIgnoresABlackScreen(t *testing.T) {
	f := running(t)
	f.SetScreenshot("vm", "image/png", screen(t, false))
	sleeps := 0
	err := console.WaitForPrompt(context.Background(), f, "vm", prompt(&sleeps))
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "nothing was typed") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitForPromptReadsPPM(t *testing.T) {
	f := running(t)
	f.SetScreenshot("vm", "image/x-portable-pixmap", append([]byte("P6\n2 1\n255\n"), 0, 0, 0, 9, 9, 9))
	sleeps := 0
	if err := console.WaitForPrompt(context.Background(), f, "vm", prompt(&sleeps)); err != nil {
		t.Error(err)
	}
	f.SetScreenshot("vm", "image/x-portable-pixmap", append([]byte("P6\n2 1\n255\n"), 0, 0, 0, 0, 0, 0))
	if err := console.WaitForPrompt(context.Background(), f, "vm", prompt(&sleeps)); !failure.Is(err, failure.Timeout) {
		t.Errorf("a black PPM counted as a prompt: %v", err)
	}
}

func TestType(t *testing.T) {
	f := running(t)
	if err := console.Type(f, "vm", "aB !"); err != nil {
		t.Fatal(err)
	}
	keys := f.SentKeys("vm")
	want := [][]uint32{{30}, {42, 48}, {57}, {42, 2}, {28}}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v", keys)
	}
	for i := range want {
		if !slices.Equal(keys[i], want[i]) {
			t.Errorf("key %d = %v, want %v", i, keys[i], want[i])
		}
	}
	if err := console.Type(f, "vm", "ä"); err == nil || len(f.SentKeys("vm")) != len(want) {
		t.Error("an untypeable password was typed in part")
	}
}

// The characters vm_test.sh types, each with its US key.
func TestTypeShiftedAndPunctuation(t *testing.T) {
	f := running(t)
	if err := console.Type(f, "vm", `Q!a-_"\`); err != nil {
		t.Fatal(err)
	}
	want := [][]uint32{{42, 16}, {42, 2}, {30}, {12}, {42, 12}, {42, 40}, {43}, {28}}
	keys := f.SentKeys("vm")
	if len(keys) != len(want) {
		t.Fatalf("keys = %v", keys)
	}
	for i := range want {
		if !slices.Equal(keys[i], want[i]) {
			t.Errorf("key %d = %v, want %v", i, keys[i], want[i])
		}
	}
}

// A cursor that blinks between two screens still lets the prompt settle.
func TestWaitForPromptSettlesWithABlinkingCursor(t *testing.T) {
	f := running(t)
	on, off := screen(t, true), screen(t, true)
	off = append(off[:len(off):len(off)], 0) // another hash, the same image
	f.SetScreenshot("vm", "image/png", on)
	shots := 0
	p := console.Prompt{Timeout: 20 * time.Second, Interval: 2 * time.Second, Still: 4, Sleep: func(time.Duration) {
		shots++
		if shots%2 == 1 {
			f.SetScreenshot("vm", "image/png", off)
		} else {
			f.SetScreenshot("vm", "image/png", on)
		}
	}}
	if err := console.WaitForPrompt(context.Background(), f, "vm", p); err != nil {
		t.Fatal(err)
	}
}

// SIGINT ends the wait for the disk prompt during its pause, and nothing is
// typed.
func TestWaitForPromptStopsOnInterrupt(t *testing.T) {
	f := running(t)
	f.SetScreenshot("vm", "image/png", screen(t, false))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := console.WaitForPrompt(ctx, f, "vm", console.Prompt{Timeout: time.Hour, Interval: time.Hour, Still: 4})
	if err == nil || !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "nothing was typed") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the wait took %v after the interrupt", d)
	}
	if len(f.SentKeys("vm")) != 0 {
		t.Error("keys were typed")
	}
}

// A console that cannot be read never counts as a prompt.
func TestWaitForPromptSkipsAFailedScreenshot(t *testing.T) {
	f := running(t)
	f.SetScreenshot("vm", "image/png", screen(t, true))
	f.FailOn("Screenshot", failure.New(failure.Failed, "no screen"))
	sleeps := 0
	if err := console.WaitForPrompt(context.Background(), f, "vm", prompt(&sleeps)); !failure.Is(err, failure.Timeout) {
		t.Errorf("err = %v", err)
	}
}

// The firmware screen of Omarchy held still for 23 s on the first boot of a
// copied disk. The default timings wait it out and type at the prompt.
func TestDefaultPromptWaitsOutTheFirmwareScreen(t *testing.T) {
	f := running(t)
	firmwareScreen, promptScreen := screen(t, true), screen(t, true)
	promptScreen = append(promptScreen[:len(promptScreen):len(promptScreen)], 0) // another hash
	f.SetScreenshot("vm", "image/png", firmwareScreen)
	shots := 0
	p := console.DefaultPrompt
	p.Sleep = func(time.Duration) {
		shots++
		if shots == 12 {
			f.SetScreenshot("vm", "image/png", promptScreen)
		}
	}
	if err := console.WaitForPrompt(context.Background(), f, "vm", p); err != nil {
		t.Fatal(err)
	}
	if shots < 12+p.Still {
		t.Errorf("returned after %d waits, on the firmware screen", shots)
	}
}

// A desktop that moves never holds still; the probe ends the wait once the
// disk is open. It runs at every fifth screenshot, not at each.
func TestWaitForPromptEndsWhenTheDiskIsOpen(t *testing.T) {
	f := running(t)
	a, b, c := screen(t, true), screen(t, true), screen(t, true)
	b = append(b[:len(b):len(b)], 0)
	c = append(c[:len(c):len(c)], 0, 0)
	frames := [][]byte{a, b, c}
	shots, probes := 0, 0
	p := console.Prompt{Timeout: time.Hour, Interval: 2 * time.Second, Still: 4,
		Sleep: func(time.Duration) {
			shots++
			f.SetScreenshot("vm", "image/png", frames[shots%3])
		},
		Opened: func() bool { probes++; return probes == 2 },
	}
	f.SetScreenshot("vm", "image/png", a)
	err := console.WaitForPrompt(context.Background(), f, "vm", p)
	if !errors.Is(err, console.ErrOpened) {
		t.Fatalf("err = %v, want ErrOpened", err)
	}
	if shots != 10 {
		t.Errorf("returned after %d waits, want the second probe at the tenth screenshot", shots)
	}
}

// A guest that is still locked at the timeout keeps the timeout error. The
// probe runs once more at the timeout.
func TestWaitForPromptProbesAtTheTimeout(t *testing.T) {
	f := running(t)
	f.SetScreenshot("vm", "image/png", screen(t, false))
	sleeps, probes := 0, 0
	p := prompt(&sleeps)
	p.Timeout = 6 * time.Second
	p.Opened = func() bool { probes++; return false }
	if err := console.WaitForPrompt(context.Background(), f, "vm", p); !failure.Is(err, failure.Timeout) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if probes != 1 {
		t.Errorf("%d probes, want one at the timeout", probes)
	}
}
