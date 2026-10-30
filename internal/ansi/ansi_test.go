package ansi_test

import (
	"bytes"
	"testing"

	"github.com/hk9890/werft/internal/ansi"
)

func TestStrip(t *testing.T) {
	for in, want := range map[string]string{
		"plain\n":                                   "plain\n",
		"\x1b[1;32mPASS\x1b[0m  checks\n":           "PASS  checks\n",
		"\x1b[2K\rprogress\n":                       "\rprogress\n",
		"\x1b]0;title\x07after":                     "after",
		"\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\": "link",
		"\x1b(Bcharset\x1b=keypad":                  "charsetkeypad",
	} {
		var out bytes.Buffer
		if _, err := ansi.Strip(&out).Write([]byte(in)); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Errorf("Strip(%q) = %q, want %q", in, out.String(), want)
		}
	}
}

func TestStripAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := ansi.Strip(&out)
	for _, part := range []string{"a\x1b", "[3", "1mb\x1b]0;t", "itle\x07c"} {
		n, err := w.Write([]byte(part))
		if err != nil || n != len(part) {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
	}
	if out.String() != "abc" {
		t.Errorf("got %q", out.String())
	}
}
