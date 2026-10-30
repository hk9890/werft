package console_test

import (
	"bytes"
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
	if err := console.WaitForPrompt(f, "vm", prompt(&sleeps)); err != nil {
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
	err := console.WaitForPrompt(f, "vm", prompt(&sleeps))
	if !failure.Is(err, failure.Timeout) || !strings.Contains(err.Error(), "nothing was typed") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitForPromptReadsPPM(t *testing.T) {
	f := running(t)
	f.SetScreenshot("vm", "image/x-portable-pixmap", append([]byte("P6\n2 1\n255\n"), 0, 0, 0, 9, 9, 9))
	sleeps := 0
	if err := console.WaitForPrompt(f, "vm", prompt(&sleeps)); err != nil {
		t.Error(err)
	}
	f.SetScreenshot("vm", "image/x-portable-pixmap", append([]byte("P6\n2 1\n255\n"), 0, 0, 0, 0, 0, 0))
	if err := console.WaitForPrompt(f, "vm", prompt(&sleeps)); !failure.Is(err, failure.Timeout) {
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
