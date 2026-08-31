package components

import (
	"context"
	"strings"
	"testing"
)

// The card's own rendering, not just the geometry under it. Series() decides
// which rows exist and Elevated() decides their colour, and both are the kind
// of thing that keeps working while quietly doing the wrong thing.

func renderResources(t *testing.T, m OverviewResources) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return OverviewResourcesBody(m).Render(context.Background(), w)
	})
}

// A subsystem that could not be read is omitted, not drawn flat at zero. On a
// machine with no /var/lib/nanokvm a zeroed Storage row reads as an empty disk.
func TestUnreadableSubsystemsAreOmittedNotZeroed(t *testing.T) {
	m := OverviewResources{
		Sampling: true,
		CPU:      SparkSeries{Label: "Processor", Value: 12, Points: []float64{10, 12}, Valid: true},
		Memory:   SparkSeries{Label: "Memory", Value: 60, Points: []float64{58, 60}, Valid: true},
		Disk:     SparkSeries{Label: "Storage"}, // never read
	}
	html := renderResources(t, m)

	if !strings.Contains(html, "Processor") || !strings.Contains(html, "Memory") {
		t.Error("a readable subsystem is missing from the card")
	}
	if strings.Contains(html, "Storage") {
		t.Error("an unreadable subsystem rendered anyway; it will read as an empty disk")
	}
	if n := strings.Count(html, "<svg"); n != 2 {
		t.Errorf("%d graphs drawn, want one per readable subsystem (2)", n)
	}
}

// Before the sampler has a trend there is nothing to draw, and an empty box
// with axes reads as "measured, and it is zero".
func TestTheCardSaysWhyItIsEmpty(t *testing.T) {
	html := renderResources(t, OverviewResources{})
	if strings.Contains(html, "<svg") {
		t.Error("drew a graph with no readings behind it")
	}
	if !strings.Contains(html, "Collecting") {
		t.Errorf("the empty card does not say why it is empty: %s", html)
	}
}

// The threshold is the only thing that distinguishes "busy" from "in trouble"
// at a glance, since all three graphs share one fixed domain.
func TestElevatedReadingsAreColouredApart(t *testing.T) {
	for _, tc := range []struct {
		pct  float64
		want bool
	}{{10, false}, {74.9, false}, {75, true}, {99, true}} {
		if got := (SparkSeries{Value: tc.pct}).Elevated(); got != tc.want {
			t.Errorf("Elevated(%v%%) = %v, want %v", tc.pct, got, tc.want)
		}
	}

	hot := OverviewResources{Sampling: true,
		CPU: SparkSeries{Label: "Processor", Value: 92, Points: []float64{90, 92}, Valid: true}}
	if !strings.Contains(renderResources(t, hot), "text-destructive") {
		t.Error("a 92% reading is not coloured apart from a quiet one")
	}
	cool := OverviewResources{Sampling: true,
		CPU: SparkSeries{Label: "Processor", Value: 9, Points: []float64{8, 9}, Valid: true}}
	if strings.Contains(renderResources(t, cool), "text-destructive") {
		t.Error("a 9% reading is coloured as elevated")
	}
}
