package presets

import "strings"

const openTag = "<presets>"

const spaceChars = " \t\r\n"

// Stripper removes a trailing <presets> block from one streamed text block.
//
// A block is only recognised when "<presets>" starts a line and is followed (after optional
// whitespace) by "[", so prose that merely mentions the tag is left alone. Text that could
// still turn into a match is held back until it resolves, and so is trailing whitespace, so
// the blank line before a block never reaches the client.
type Stripper struct {
	pending     string
	atLineStart bool // whether the last emitted byte ended a line (or nothing emitted yet)
	detected    bool
	gap         string // whitespace between the visible text and the tag
	captured    strings.Builder
}

// NewStripper returns a stripper for a fresh text block. Block starts count as line starts.
func NewStripper() *Stripper {
	return &Stripper{atLineStart: true}
}

// Feed consumes a text delta and returns the text that is safe to forward. Once detected
// is true, everything fed afterwards is captured instead of emitted.
func (s *Stripper) Feed(text string) (emit string, detected bool) {
	if s.detected {
		s.captured.WriteString(text)
		return "", true
	}
	p := s.pending + text
	for i := 0; i < len(p); i++ {
		if p[i] != '<' {
			continue
		}
		if !((i == 0 && s.atLineStart) || (i > 0 && p[i-1] == '\n')) {
			continue
		}
		switch matchTag(p[i:]) {
		case matchFull:
			visible := strings.TrimRight(p[:i], spaceChars)
			s.detected = true
			s.pending = ""
			s.gap = p[len(visible):i]
			s.captured.WriteString(p[i+len(openTag):])
			return visible, true
		case matchPartial:
			cut := len(strings.TrimRight(p[:i], spaceChars))
			return s.emit(p, cut), false
		}
	}
	return s.emit(p, len(strings.TrimRight(p, spaceChars))), false
}

func (s *Stripper) emit(p string, cut int) string {
	out := p[:cut]
	s.pending = p[cut:]
	if out != "" {
		s.atLineStart = strings.HasSuffix(out, "\n")
	}
	return out
}

// Flush returns any held-back text at the end of the block. It returns "" after detection.
func (s *Stripper) Flush() string {
	if s.detected {
		return ""
	}
	out := s.pending
	s.pending = ""
	return out
}

func (s *Stripper) Detected() bool { return s.detected }

// Captured returns everything after the opening tag (the JSON array, closing tag, and any
// trailing text).
func (s *Stripper) Captured() string { return s.captured.String() }

// Suffix returns exactly what was stripped: the whitespace before the tag, the tag, and
// everything after it. Visible text + Suffix reproduces the model's output.
func (s *Stripper) Suffix() string { return s.gap + openTag + s.captured.String() }

type match int

const (
	matchNone match = iota
	matchPartial
	matchFull
)

func matchTag(s string) match {
	n := min(len(s), len(openTag))
	if s[:n] != openTag[:n] {
		return matchNone
	}
	if len(s) < len(openTag) {
		return matchPartial
	}
	j := len(openTag)
	for j < len(s) && strings.IndexByte(spaceChars, s[j]) >= 0 {
		j++
	}
	switch {
	case j == len(s):
		return matchPartial
	case s[j] == '[':
		return matchFull
	default:
		return matchNone
	}
}

// StripText applies the stripper to a complete text (non-streaming responses).
func StripText(text string) (visible, captured, suffix string, found bool) {
	s := NewStripper()
	visible, found = s.Feed(text)
	visible += s.Flush()
	return visible, s.Captured(), s.Suffix(), found
}
