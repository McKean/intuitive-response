package presets

import (
	"strings"
	"testing"
)

// feedChunks runs the stripper over chunks and returns the emitted text.
func feedChunks(chunks []string) (string, *Stripper) {
	s := NewStripper()
	var out strings.Builder
	for _, c := range chunks {
		emit, _ := s.Feed(c)
		out.WriteString(emit)
	}
	out.WriteString(s.Flush())
	return out.String(), s
}

func TestStripperEverySplit(t *testing.T) {
	answer := "Use Postgres — it handles the <b>concurrency</b>.\nDone."
	block := `<presets>[{"expected_input":"asks why not SQLite","response":"SQLite locks.","max_turns":1}]</presets>`
	full := answer + "\n\n" + block
	// Every two-way and three-way split must give the same visible text and capture.
	for i := 0; i <= len(full); i++ {
		for j := i; j <= len(full); j++ {
			got, s := feedChunks([]string{full[:i], full[i:j], full[j:]})
			if got != answer {
				t.Fatalf("split %d/%d: visible = %q, want %q", i, j, got, answer)
			}
			if !s.Detected() {
				t.Fatalf("split %d/%d: not detected", i, j)
			}
			if items, err := Parse(s.Captured()); err != nil || len(items) != 1 {
				t.Fatalf("split %d/%d: parse = %v, %v", i, j, items, err)
			}
			if got+s.Suffix() != full {
				t.Fatalf("split %d/%d: visible+suffix does not reproduce the output", i, j)
			}
		}
	}
}

func TestStripperByteByByte(t *testing.T) {
	full := "Héllo 👋\n<presets>\n  [ ]</presets>"
	var chunks []string
	for i := range len(full) {
		chunks = append(chunks, full[i:i+1])
	}
	got, s := feedChunks(chunks)
	if got != "Héllo 👋" || !s.Detected() {
		t.Fatalf("visible = %q detected = %v", got, s.Detected())
	}
}

func TestStripperLeavesProseAlone(t *testing.T) {
	cases := []string{
		"The proxy looks for a <presets> tag.",        // not at line start
		"Mention:\n<presets> is the tag name.",        // not followed by [
		"Code:\n<presets\nnot closed",                 // partial tag, then diverges
		"Trailing whitespace is kept at the end.\n\n", // flushed at block end
		"Ends with a partial tag\n<pres",              // flushed at block end
		"```\n<presetsX>[1]\n```",                     // different tag
		"  <presets>[] indented is not at line start", // leading spaces
	}
	for _, c := range cases {
		for split := 0; split <= len(c); split++ {
			got, s := feedChunks([]string{c[:split], c[split:]})
			if got != c || s.Detected() {
				t.Errorf("%q split %d: got %q detected=%v", c, split, got, s.Detected())
			}
		}
	}
}

func TestStripperBlockAtStart(t *testing.T) {
	got, s := feedChunks([]string{"<pre", "sets>[]</presets>"})
	if got != "" || !s.Detected() {
		t.Fatalf("got %q detected=%v", got, s.Detected())
	}
}

func TestStripperStreamsWithoutHolding(t *testing.T) {
	s := NewStripper()
	if emit, _ := s.Feed("Hello world"); emit != "Hello world" {
		t.Fatalf("plain text held back: %q", emit)
	}
	if emit, _ := s.Feed(" and more\n"); emit != " and more" {
		t.Fatalf("got %q; only the trailing newline should be held", emit)
	}
	if emit, _ := s.Feed("next line"); emit != "\nnext line" {
		t.Fatalf("got %q", emit)
	}
}

func TestStripText(t *testing.T) {
	vis, capt, _, found := StripText("Answer.\n<presets>[{\"expected_input\":\"a\",\"response\":\"b\"}]</presets>\n")
	if !found || vis != "Answer." || !strings.HasPrefix(capt, "[") {
		t.Fatalf("got %q %q %v", vis, capt, found)
	}
}
