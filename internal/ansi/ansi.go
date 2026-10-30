// Package ansi removes terminal escape sequences from a stream, so a log
// file holds plain text.
package ansi

import "io"

type state int

const (
	text state = iota
	escape
	intermediate
	csi
	osc
	oscEscape
)

const esc = 0x1b

// Writer passes what is written to it on to w without CSI sequences
// (colours, cursor moves), OSC sequences (window titles, links) and the
// other ESC sequences. A sequence can span writes.
type Writer struct {
	w     io.Writer
	state state
}

func Strip(w io.Writer) *Writer { return &Writer{w: w} }

func (s *Writer) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		switch s.state {
		case text:
			if b == esc {
				s.state = escape
			} else {
				out = append(out, b)
			}
		case escape:
			switch {
			case b == '[':
				s.state = csi
			case b == ']':
				s.state = osc
			case b >= 0x20 && b <= 0x2f:
				s.state = intermediate
			default:
				s.state = text
			}
		case intermediate:
			if b < 0x20 || b > 0x2f {
				s.state = text
			}
		case csi:
			if b >= 0x40 && b <= 0x7e {
				s.state = text
			}
		case osc:
			switch b {
			case 0x07:
				s.state = text
			case esc:
				s.state = oscEscape
			}
		case oscEscape:
			s.state = text
		}
	}
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
