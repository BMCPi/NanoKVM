package components

// overview_resources.go backs the Server Overview's Resources card: the model
// the fragment fills in, and the geometry for the sparklines.
//
// The graphs are hand-drawn SVG rather than the vendored chart component, for
// one reason: they plot percentages, and a percentage graph has to be pinned to
// 0..100. The chart's client does support a fixed ceiling — chart.js reads
// m.domainMax and lays the ticks out against it — but nothing in chart.templ
// puts that field in the model, so it is unreachable from Go and the domain
// comes from domainTicks(), which derives it from the data. An idle BMC
// drifting between 1% and 3% would therefore fill the plot and read as a
// machine under load. Pinned, it reads as the flat line it is.
//
// Drawing them here also keeps three ResizeObserver-driven chart instances out
// of a drawer on a 1 GHz single core, and means the graphs arrive fully drawn
// in the fragment's HTML with no client work at all.

import (
	"math"
	"strconv"
	"strings"
)

// OverviewResources is the Resources card body.
type OverviewResources struct {
	// Sampling is false until the sampler has enough readings to draw a
	// trend, which is the card's whole content — see the empty state.
	Sampling bool
	CPU      ResourceSeries
	Memory   ResourceSeries
	Disk     ResourceSeries
}

// ResourceSeries is one measured resource: its current value, the trend behind
// it, and the absolute figures that give the percentage meaning.
type ResourceSeries struct {
	Label string
	// Percent is the latest reading, 0..100.
	Percent float64
	// Detail is the absolute figure beside the percentage ("161 / 246 MB").
	// Empty for CPU, which has no total worth stating.
	Detail string
	// Points is the history, oldest first, as percentages.
	Points []float64
	// Valid is false when the subsystem could not be read at all — a dev
	// workstation with no /var/lib/nanokvm, say. The row is omitted rather
	// than drawn as a flat zero.
	Valid bool
}

// PercentLabel is the current reading for display.
func (s ResourceSeries) PercentLabel() string {
	return strconv.FormatFloat(s.Percent, 'f', 0, 64) + "%"
}

// Elevated reports whether the reading is high enough to colour. Three
// quarters is the point at which a BMC's data volume or memory stops being
// something an operator can ignore, and it is the same threshold for all three
// so the card does not need a legend to explain itself.
func (s ResourceSeries) Elevated() bool { return s.Percent >= 75 }

// Series is the rows in the order they are drawn, skipping any subsystem that
// could not be read.
func (m OverviewResources) Series() []ResourceSeries {
	out := make([]ResourceSeries, 0, 3)
	for _, s := range []ResourceSeries{m.CPU, m.Memory, m.Disk} {
		if s.Valid {
			out = append(out, s)
		}
	}
	return out
}

// The sparkline viewBox. Square and unitless: the SVG is stretched to whatever
// the card gives it with preserveAspectRatio="none", and the stroke is held at
// one device pixel with vector-effect so the distortion never shows.
const (
	sparkWidth  = 100.0
	sparkHeight = 100.0
)

// sparkPolyline maps percentages onto the viewBox as "x,y x,y …".
//
// y is inverted (0% at the bottom) and the domain is fixed: 0 is the floor of
// the box and 100 its ceiling, whatever the data does. That fixed domain is the
// point of this function.
func sparkPolyline(values []float64) string {
	if len(values) == 0 {
		return ""
	}
	// A lone reading has no run to draw, so it becomes a flat line across the
	// box rather than a single invisible point.
	if len(values) == 1 {
		y := sparkY(values[0])
		return "0," + y + " " + trim(sparkWidth) + "," + y
	}

	var sb strings.Builder
	last := float64(len(values) - 1)
	for i, v := range values {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(trim(float64(i) / last * sparkWidth))
		sb.WriteByte(',')
		sb.WriteString(sparkY(v))
	}
	return sb.String()
}

// sparkPolygon is sparkPolyline closed down to the baseline, for the fill under
// the trace.
func sparkPolygon(values []float64) string {
	line := sparkPolyline(values)
	if line == "" {
		return ""
	}
	base := trim(sparkHeight)
	return "0," + base + " " + line + " " + trim(sparkWidth) + "," + base
}

// sparkY maps a percentage to a y coordinate, clamped to the box.
func sparkY(v float64) string {
	return trim(sparkHeight - clampPercent(v)/100*sparkHeight)
}

// clampPercent keeps a reading inside the fixed domain. The collector clamps
// too (pkg/sysinfo), but this function is what stops a bad number drawing
// outside the box, and it is cheap enough to not make that someone else's job.
func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

// trim formats a coordinate rounded to two decimals with no trailing zeros.
//
// The rounding is not cosmetic. A 180-point trace divides the width by 179, so
// the shortest round-tripping form of each x is seventeen digits, and three
// traces of those would add tens of kilobytes to a fragment that re-renders
// every time the drawer opens. Two decimals is a hundredth of the viewBox —
// far finer than the drawer can display.
func trim(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}
