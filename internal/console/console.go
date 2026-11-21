// Package console types at the boot console of a domain: it waits for a
// prompt on the screen and sends key codes, with no virsh process, so no
// keystroke shows in a process list.
package console

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"
	"time"

	"github.com/hk9890/werft/internal/failure"
	"github.com/hk9890/werft/internal/virt"
)

// Linux input key codes.
const (
	keyLeftShift = 42
	keyEnter     = 28
	keySpace     = 57
)

var letters = map[rune]uint32{
	'q': 16, 'w': 17, 'e': 18, 'r': 19, 't': 20, 'y': 21, 'u': 22, 'i': 23, 'o': 24, 'p': 25,
	'a': 30, 's': 31, 'd': 32, 'f': 33, 'g': 34, 'h': 35, 'j': 36, 'k': 37, 'l': 38,
	'z': 44, 'x': 45, 'c': 46, 'v': 47, 'b': 48, 'n': 49, 'm': 50,
}

// The key at index i types usPlain[i], and with shift usShifted[i], on a US
// keyboard: the keymap of the guests' boot console.
var (
	usKeys    = []uint32{41, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 26, 27, 43, 39, 40, 51, 52, 53}
	usPlain   = "`1234567890-=[]\\;',./"
	usShifted = "~!@#$%^&*()_+{}|:\"<>?"
)

// Chord is the keys pressed together to type r on a US keyboard.
func Chord(r rune) ([]uint32, bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return []uint32{letters[r]}, true
	case r >= 'A' && r <= 'Z':
		return []uint32{keyLeftShift, letters[r-'A'+'a']}, true
	case r == ' ':
		return []uint32{keySpace}, true
	}
	if i := strings.IndexRune(usPlain, r); i >= 0 {
		return []uint32{usKeys[i]}, true
	}
	if i := strings.IndexRune(usShifted, r); i >= 0 {
		return []uint32{keyLeftShift, usKeys[i]}, true
	}
	return nil, false
}

// Typeable fails when a character of s has no key on a US keyboard. The
// error does not name the character, since s is a secret.
func Typeable(s string) error {
	for _, r := range s {
		if _, ok := Chord(r); !ok {
			return failure.New(failure.Usage, "the password has a character no key of a US keyboard types, so it cannot be typed at a boot console")
		}
	}
	return nil
}

// Prompt is how long and how often to look for a prompt.
type Prompt struct {
	// Timeout counts the intervals between the screenshots only: each call
	// of Opened adds its own time, so the wait can take longer.
	Timeout  time.Duration
	Interval time.Duration
	// Still is how many screenshots in a row must not change before the
	// prompt counts as up.
	Still int
	// Sleep pauses between two screenshots. When nil, the wait sleeps on
	// its own and ends early when the context ends.
	Sleep func(time.Duration)
	// Opened, when set, reports whether the disk is open already, as when a
	// person typed the password in a viewer: a desktop that moves never
	// holds still. A probe can take seconds, so the wait asks it at every
	// openedEvery-th screenshot and at the timeout, not at each.
	Opened func() bool
}

const openedEvery = 5

// ErrOpened is the error of WaitForPrompt when Prompt.Opened reports the disk
// open before a prompt showed.
var ErrOpened = failure.New(failure.Failed, "the disk is open already; nothing was typed")

// DefaultPrompt waits for a screen that holds still for 40 s. The prompt
// waits as long as it takes; the firmware screen of Omarchy held still for
// 23 s on the first boot of a copied disk, and vm's 8 s typed into it.
var DefaultPrompt = Prompt{Timeout: 180 * time.Second, Interval: 2 * time.Second, Still: 20}

func (p Prompt) sleep(ctx context.Context) {
	if p.Sleep != nil {
		p.Sleep(p.Interval)
		return
	}
	t := time.NewTimer(p.Interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// WaitForPrompt returns once the console shows something other than black
// and holds still: the prompt waits, the boot stages before it move on. A
// screenshot that matches either of the two before it counts as still, so a
// blinking cursor cannot keep the prompt from settling. It returns ErrOpened
// when p.Opened reports the disk open. It stops when ctx ends, as on SIGINT.
func WaitForPrompt(ctx context.Context, h virt.Hypervisor, domain string, p Prompt) error {
	var last, before [32]byte
	still := 0
	// lit keeps the verdict of blank per screen, so a still screen is
	// decoded once, not on each of its screenshots.
	lit := map[[32]byte]bool{}
	for shot, waited := 0, time.Duration(0); ; shot, waited = shot+1, waited+p.Interval {
		if err := ctx.Err(); err != nil {
			return failure.Wrap(failure.Failed, err, "interrupted while waiting for the prompt of %s; nothing was typed", domain)
		}
		probe := shot > 0 && (shot%openedEvery == 0 || waited >= p.Timeout)
		if probe && p.Opened != nil && p.Opened() {
			return ErrOpened
		}
		var buf bytes.Buffer
		if _, err := h.Screenshot(domain, &buf); err == nil {
			sum := sha256.Sum256(buf.Bytes())
			if _, ok := lit[sum]; !ok {
				lit[sum] = !blank(buf.Bytes())
			}
			if (sum == last || sum == before) && lit[sum] {
				still++
			} else {
				still = 0
			}
			before, last = last, sum
			if still >= p.Still {
				return nil
			}
		}
		if waited >= p.Timeout {
			return failure.New(failure.Timeout, "the console of %s did not settle on a prompt within %v; nothing was typed", domain, p.Timeout)
		}
		p.sleep(ctx)
	}
}

// Type types s and Enter, one key chord per character.
func Type(h virt.Hypervisor, domain, s string) error {
	if err := Typeable(s); err != nil {
		return err
	}
	for _, r := range s {
		chord, _ := Chord(r)
		if err := h.SendKey(domain, chord); err != nil {
			return failure.New(failure.KindOf(err), "could not type at the console of %s", domain)
		}
	}
	if err := h.SendKey(domain, []uint32{keyEnter}); err != nil {
		return failure.New(failure.KindOf(err), "could not type at the console of %s", domain)
	}
	return nil
}

// blank reports whether a PNG or PPM screenshot is all black. A screenshot
// that does not decode counts as blank, so it never counts as a prompt.
func blank(data []byte) bool {
	img, err := decode(data)
	if err != nil {
		return true
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if r, g, bl, _ := img.At(x, y).RGBA(); r|g|bl != 0 {
				return false
			}
		}
	}
	return true
}

func decode(data []byte) (image.Image, error) {
	if bytes.HasPrefix(data, []byte("P6")) {
		return decodePPM(data)
	}
	return png.Decode(bytes.NewReader(data))
}

// decodePPM reads a binary PPM with 8-bit samples, the format QEMU writes
// when it cannot write PNG.
func decodePPM(data []byte) (image.Image, error) {
	r := bufio.NewReader(bytes.NewReader(data))
	var magic string
	var w, h, maxval int
	if _, err := fmt.Fscan(r, &magic, &w, &h, &maxval); err != nil {
		return nil, fmt.Errorf("PPM header: %w", err)
	}
	if maxval != 255 || w <= 0 || h <= 0 {
		return nil, fmt.Errorf("unsupported PPM %dx%d, maxval %d", w, h, maxval)
	}
	if _, err := r.ReadByte(); err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	px := make([]byte, 3)
	for i := 0; i < w*h; i++ {
		if _, err := io.ReadFull(r, px); err != nil {
			return nil, fmt.Errorf("PPM pixels: %w", err)
		}
		copy(img.Pix[i*4:], px)
		img.Pix[i*4+3] = 0xff
	}
	return img, nil
}
