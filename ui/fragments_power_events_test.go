package ui

import (
	"strings"
	"testing"
)

// A rendered templ fragment contains newlines; SSE frames cannot. Anything
// that reaches a data: line with a raw \n truncates the event silently and
// the client swaps a partial fragment.
func TestPowerEventFramesAreSingleLine(t *testing.T) {
	frame := powerEventFrame("powertoggle", "<button>\n  <span>hi</span>\n</button>")

	lines := strings.Split(strings.TrimSuffix(frame, "\n\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frame has %d lines, want exactly an event: and a data: line:\n%q",
			len(lines), frame)
	}
	if !strings.HasPrefix(lines[0], "event: powertoggle") {
		t.Errorf("first line = %q, want the event name", lines[0])
	}
	if !strings.HasPrefix(lines[1], "data: ") {
		t.Errorf("second line = %q, want a data: line", lines[1])
	}
	if strings.Contains(lines[1], "\n") {
		t.Error("data line still carries a raw newline")
	}
}

func TestPowerEventFrameEndsWithBlankLine(t *testing.T) {
	if !strings.HasSuffix(powerEventFrame("powerpill", "<span/>"), "\n\n") {
		t.Error("SSE frames must be terminated by a blank line")
	}
}
