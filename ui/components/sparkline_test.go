package components

import (
	"strconv"
	"strings"
	"testing"
)

// sparkline_test.go guards the trace geometry.
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
	got := polylineAt100([]float64{1, 3, 2})

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
	pts := strings.Fields(polylineAt100([]float64{10, 20, 30, 40, 50}))
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
	pts := strings.Fields(polylineAt100([]float64{0, 100}))
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
	pts := strings.Fields(polylineAt100([]float64{42}))
	if len(pts) != 2 {
		t.Fatalf("a single reading produced %d points (%v), want a flat 2-point line", len(pts), pts)
	}
	if pts[0] != "0,58" || pts[1] != "100,58" {
		t.Errorf("flat line = %v, want 0,58 and 100,58", pts)
	}
}

func TestSparklineIsEmptyWithNoReadings(t *testing.T) {
	if got := polylineAt100(nil); got != "" {
		t.Errorf("polylineAt100(nil) = %q, want empty", got)
	}
	if got := polygonAt100(nil); got != "" {
		t.Errorf("polygonAt100(nil) = %q, want empty", got)
	}
}

// The fill is the same trace closed down to the baseline; if it does not close
// there, the area paints as a filled outline of the line itself.
func TestSparkPolygonClosesToTheBaseline(t *testing.T) {
	got := polygonAt100([]float64{50, 60})
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
	pts := strings.Fields(polylineAt100([]float64{-20, 140}))
	if pts[0] != "0,100" || pts[1] != "100,0" {
		t.Errorf("out-of-range readings = %v, want them clamped to 0,100 and 100,0", pts)
	}
}

// polylineAt100 / polygonAt100 pin the domain at 100 for the tests above, which
// are about the mapping rather than about the ceiling. The ceiling gets its own
// test below.
func polylineAt100(values []float64) string { return sparkPolyline(values, 100) }
func polygonAt100(values []float64) string  { return sparkPolygon(values, 100) }

// The domain is a property of the series, not a constant: a percentage tops out
// at 100 and a die temperature is drawn against the same box, but a series with
// its own ceiling has to scale to that instead.
func TestTheCeilingScalesTheTrace(t *testing.T) {
	// 40 against a ceiling of 80 is half-height; against 100 it is not.
	if got := sparkPolyline([]float64{40}, 80); !strings.Contains(got, ",50") {
		t.Errorf("40 against a ceiling of 80 = %q, want it at the midpoint y=50", got)
	}
	if got := sparkPolyline([]float64{40}, 100); !strings.Contains(got, ",60") {
		t.Errorf("40 against a ceiling of 100 = %q, want y=60", got)
	}
}

func TestSeriesDefaultsToAHundredCeiling(t *testing.T) {
	if got := (SparkSeries{}).Ceiling(); got != 100 {
		t.Errorf("Ceiling() = %v with no Max set, want 100", got)
	}
	if got := (SparkSeries{Max: 80}).Ceiling(); got != 80 {
		t.Errorf("Ceiling() = %v, want the series' own 80", got)
	}
}

// The marker is the reference the trace is read against — the point at which
// the host starts capping itself. It has to sit at that value, and it has to
// disappear when there is none or when it would land on the frame.
func TestTheMarkerSitsAtItsValue(t *testing.T) {
	s := SparkSeries{Max: 100, Marker: 80}
	if !s.HasMarker() {
		t.Fatal("HasMarker() = false with a marker set")
	}
	if got := s.MarkerY(); got != "20" {
		t.Errorf("MarkerY() = %q for 80 of 100, want 20", got)
	}
	for _, tc := range []SparkSeries{{Max: 100}, {Max: 100, Marker: 100}, {Max: 100, Marker: 140}} {
		if tc.HasMarker() {
			t.Errorf("HasMarker() = true for %+v; a marker at or past the ceiling is the frame", tc)
		}
	}
}

// The colour threshold follows the ceiling, so a temperature series does not
// inherit a percentage's 75.
func TestElevatedFollowsTheSeriesThreshold(t *testing.T) {
	if !(SparkSeries{Value: 76}).Elevated() {
		t.Error("76 of a default 100 ceiling should be elevated")
	}
	if (SparkSeries{Value: 60, Max: 100, WarnAt: 80}).Elevated() {
		t.Error("60 is below an explicit 80 threshold")
	}
	if !(SparkSeries{Value: 82, Max: 100, WarnAt: 80}).Elevated() {
		t.Error("82 is above an explicit 80 threshold")
	}
}

func TestValueLabelCarriesTheUnit(t *testing.T) {
	for _, tc := range []struct {
		s    SparkSeries
		want string
	}{
		{SparkSeries{Value: 12.4, Unit: "%"}, "12%"},
		{SparkSeries{Value: 12.6, Unit: "%"}, "13%"},
		{SparkSeries{Value: 47.2, Unit: "°C"}, "47°C"},
	} {
		if got := tc.s.ValueLabel(); got != tc.want {
			t.Errorf("ValueLabel() = %q, want %q", got, tc.want)
		}
	}
}

// A high reading is not always a problem. A cooler at 92% duty is the system
// working; colouring it as a fault trains the operator to ignore the colour on
// the temperature row above it, where it does mean something.
func TestNoWarnSeriesNeverColours(t *testing.T) {
	fan := SparkSeries{Value: 100, Unit: "%", WarnAt: NoWarn}
	if fan.Elevated() {
		t.Error("a NoWarn series was coloured at 100")
	}
	// And the default still applies where no threshold is given.
	if !(SparkSeries{Value: 100}).Elevated() {
		t.Error("NoWarn leaked into the default threshold")
	}
}
