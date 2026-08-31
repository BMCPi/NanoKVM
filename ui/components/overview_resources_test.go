package components

import (
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
