package components

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// overview_resources_test.go guards the sparkline geometry.
//
// These graphs plot percentages, and the whole reason they are hand-drawn
// rather than handed to the vendored chart component is that the y axis must
// be pinned to 0..100 — the chart's Go API exposes no domain override, so it
// scales to the data and an idle BMC wobbling between 1% and 3% would fill the
// plot and read as a machine in trouble. If that pinning ever breaks, the graphs become actively misleading
// rather than merely wrong, so it is the thing tested hardest here.

func TestSparklineIsPinnedToZeroAndOneHundred(t *testing.T) {
	// Three readings that span a narrow band. On an auto-scaled axis these
	// would fill the plot; pinned, they must all sit near the bottom.
	got := sparkPolyline([]float64{1, 3, 2})

	pts := strings.Fields(got)
	if len(pts) != 3 {
		t.Fatalf("polyline = %q, want 3 points", got)
	}
	for _, p := range pts {
		_, ys, ok := strings.Cut(p, ",")
		if !ok {
			t.Fatalf("malformed point %q in %q", p, got)
		}
		y, err := strconv.ParseFloat(ys, 64)
		if err != nil {
			t.Fatalf("unparseable y in %q: %v", p, err)
		}
		// y is inverted: 0% is at y=100, 100% at y=0. A 1-3% reading must
		// land in the bottom 5% of the box.
		if y < 95 {
			t.Errorf("point %q sits at y=%v; a 1-3%% reading should be near the "+
				"baseline, not scaled to fill the plot", p, y)
		}
	}
}

func TestSparklineSpansTheFullWidth(t *testing.T) {
	pts := strings.Fields(sparkPolyline([]float64{10, 20, 30, 40, 50}))
	if first := pts[0]; !strings.HasPrefix(first, "0,") {
		t.Errorf("first point %q does not start at x=0", first)
	}
	if last := pts[len(pts)-1]; !strings.HasPrefix(last, "100,") {
		t.Errorf("last point %q does not reach x=100", last)
	}
}

// A hundred percent must touch the top of the box and zero the bottom, or the
// reader cannot tell a saturated resource from a busy one.
func TestSparklineEndpointsMapToTheBoxEdges(t *testing.T) {
	pts := strings.Fields(sparkPolyline([]float64{0, 100}))
	if pts[0] != "0,100" {
		t.Errorf("0%% mapped to %q, want 0,100", pts[0])
	}
	if pts[1] != "100,0" {
		t.Errorf("100%% mapped to %q, want 100,0", pts[1])
	}
}

// One reading is a value, not a trend; it still has to draw something, and a
// single point draws nothing at all.
func TestSparklineDrawsAFlatLineForASingleReading(t *testing.T) {
	pts := strings.Fields(sparkPolyline([]float64{42}))
	if len(pts) != 2 {
		t.Fatalf("a single reading produced %d points (%v), want a flat 2-point line", len(pts), pts)
	}
	if pts[0] != "0,58" || pts[1] != "100,58" {
		t.Errorf("flat line = %v, want 0,58 and 100,58", pts)
	}
}

func TestSparklineIsEmptyWithNoReadings(t *testing.T) {
	if got := sparkPolyline(nil); got != "" {
		t.Errorf("sparkPolyline(nil) = %q, want empty", got)
	}
	if got := sparkPolygon(nil); got != "" {
		t.Errorf("sparkPolygon(nil) = %q, want empty", got)
	}
}

// The fill is the same trace closed down to the baseline; if it does not close
// there, the area paints as a filled outline of the line itself.
func TestSparkPolygonClosesToTheBaseline(t *testing.T) {
	got := sparkPolygon([]float64{50, 60})
	if !strings.HasPrefix(got, "0,100 ") {
		t.Errorf("polygon %q does not open on the baseline", got)
	}
	if !strings.HasSuffix(got, " 100,100") {
		t.Errorf("polygon %q does not close on the baseline", got)
	}
	if !strings.Contains(got, "0,50") || !strings.Contains(got, "100,40") {
		t.Errorf("polygon %q lost the trace between its baseline anchors", got)
	}
}

// A reading outside 0..100 would draw outside the box it is pinned to.
func TestSparklineClampsOutOfRangeReadings(t *testing.T) {
	pts := strings.Fields(sparkPolyline([]float64{-20, 140}))
	if pts[0] != "0,100" || pts[1] != "100,0" {
		t.Errorf("out-of-range readings = %v, want them clamped to 0,100 and 100,0", pts)
	}
}

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
		CPU:      ResourceSeries{Label: "Processor", Percent: 12, Points: []float64{10, 12}, Valid: true},
		Memory:   ResourceSeries{Label: "Memory", Percent: 60, Points: []float64{58, 60}, Valid: true},
		Disk:     ResourceSeries{Label: "Storage"}, // never read
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
		if got := (ResourceSeries{Percent: tc.pct}).Elevated(); got != tc.want {
			t.Errorf("Elevated(%v%%) = %v, want %v", tc.pct, got, tc.want)
		}
	}

	hot := OverviewResources{Sampling: true,
		CPU: ResourceSeries{Label: "Processor", Percent: 92, Points: []float64{90, 92}, Valid: true}}
	if !strings.Contains(renderResources(t, hot), "text-destructive") {
		t.Error("a 92% reading is not coloured apart from a quiet one")
	}
	cool := OverviewResources{Sampling: true,
		CPU: ResourceSeries{Label: "Processor", Percent: 9, Points: []float64{8, 9}, Valid: true}}
	if strings.Contains(renderResources(t, cool), "text-destructive") {
		t.Error("a 9% reading is coloured as elevated")
	}
}

func TestPercentLabelRounds(t *testing.T) {
	for in, want := range map[float64]string{0: "0%", 12.4: "12%", 12.6: "13%", 100: "100%"} {
		if got := (ResourceSeries{Percent: in}).PercentLabel(); got != want {
			t.Errorf("PercentLabel(%v) = %q, want %q", in, got, want)
		}
	}
}
