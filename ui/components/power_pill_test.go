package components

// power_pill_test.go guards the pairing of the power indicator's two halves.
//
// PowerPill renders a tone (colour) and a label, and they must always agree:
// a green icon beside the words "Power Off" is worse than either alone. They
// used to be paired by copy-paste across three near-identical branches, which
// is the same shape as the bug where the toggle's label and its hx-post path
// drifted apart. These tests pin every state's pairing so a fourth state
// cannot be added to one half and forgotten in the other.
//
// The indicator is also deliberately ONE fragment. Splitting the tone and the
// label into separate sse-swap targets would let them tear — the icon flips
// while the words lag a frame, or one event is dropped and they disagree
// until the next transition. TestPowerPillIsASingleFragment holds that line.

import (
	"context"
	"strings"
	"testing"
)

func renderPowerPill(t *testing.T, on, known bool) string {
	t.Helper()

	var sb strings.Builder
	if err := PowerPill(on, known).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func TestPowerPillPairsToneWithLabel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		on, known  bool
		wantLabel  string
		wantTone   string
		rejectTone string
	}{
		{
			name: "powered on", on: true, known: true,
			wantLabel: "Power On", wantTone: "fill-green-500 stroke-green-500",
			rejectTone: "fill-destructive",
		},
		{
			name: "powered off", on: false, known: true,
			wantLabel: "Power Off", wantTone: "fill-destructive stroke-destructive",
			rejectTone: "fill-green-500",
		},
		{
			// Before the stream delivers, the state is genuinely unknown —
			// it must not read as "off", which would have an operator
			// power-cycling a running host.
			name: "state not yet known", on: false, known: false,
			wantLabel: "Checking", wantTone: "fill-muted-foreground stroke-muted-foreground",
			rejectTone: "fill-destructive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := renderPowerPill(t, tc.on, tc.known)

			if !strings.Contains(html, tc.wantLabel) {
				t.Errorf("label missing %q\ngot: %s", tc.wantLabel, html)
			}
			if !strings.Contains(html, tc.wantTone) {
				t.Errorf("tone missing %q\ngot: %s", tc.wantTone, html)
			}
			if strings.Contains(html, tc.rejectTone) {
				t.Errorf("tone %q leaked into the %s state — colour and label disagree",
					tc.rejectTone, tc.name)
			}
		})
	}
}

// Colour must never be the only carrier of state (WCAG 1.4.1, and red/green
// is the most common colour-blind pair). Every state keeps readable words.
func TestPowerPillNeverReliesOnColourAlone(t *testing.T) {
	for _, known := range []bool{true, false} {
		for _, on := range []bool{true, false} {
			html := renderPowerPill(t, on, known)
			if !strings.Contains(html, "power-text") {
				t.Errorf("on=%v known=%v: no text element; state would be colour-only",
					on, known)
			}
		}
	}
}

// One fragment, so a swap updates tone and label atomically.
func TestPowerPillIsASingleFragment(t *testing.T) {
	html := renderPowerPill(t, true, true)

	if strings.Contains(html, "sse-swap") || strings.Contains(html, "sse-connect") {
		t.Error("PowerPill carries its own SSE wiring: the swap scope belongs on " +
			"the wrapper in PowerMenu, or the connection is torn down on every " +
			"state change")
	}
	if strings.Contains(html, "fill-green-500") && !strings.Contains(html, "Power On") {
		t.Error("tone rendered without its label")
	}
}
