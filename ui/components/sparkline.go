package components

// sparkline.go is the drawer's trend graph: a fixed-domain trace drawn as
// server-rendered SVG.
//
// Hand-drawn rather than handed to the vendored chart component because the
// domain has to be fixed. The chart's client does support a ceiling — chart.js
// reads m.domainMax and lays the ticks out against it — but nothing in
// chart.templ puts that field in the model, so it is unreachable from Go and
// the domain comes from domainTicks(), which derives it from the data. Against
// a derived domain an idle BMC drifting between 1% and 3% fills the plot and
// reads as a machine under load, and a host sitting at 45 °C looks identical to
// one at 82 °C. Against a fixed one, each reads as what it is.
//
// Drawing them here also keeps a ResizeObserver-driven chart instance per graph
// out of a drawer on a 1 GHz single core, and means the graphs arrive fully
// drawn in the fragment's HTML with no client work at all.

import (
	"math"
	"strconv"
	"strings"
)

// SparkSeries is one labelled trace: a current reading, the history behind it,
// and the fixed domain both are drawn against.
type SparkSeries struct {
	Label string
	// Value is the latest reading, in the series' own units.
	Value float64
	// Unit is appended to the reading ("%", "°C").
	Unit string
	// Detail is the secondary figure beside the reading ("161 / 246 MB",
	// "1400 rpm"). Empty when there is nothing useful to add.
	Detail string
	// Points is the history, oldest first.
	Points []float64
	// Valid is false when the source could not be read at all. The row is
	// omitted rather than drawn flat at zero, because a zeroed reading is a
	// reading and says something untrue.
	Valid bool

	// Max is the top of the domain. Zero means 100, which suits both a
	// percentage and a die temperature in Celsius.
	Max float64
	// WarnAt is the reading at or above which the trace is coloured. Zero
	// means three quarters of the ceiling.
	WarnAt float64
	// Marker draws a reference hairline at this reading — the point at which
	// a host starts capping itself, say. Zero draws none.
	Marker float64
}

// Ceiling is the top of the fixed domain.
func (s SparkSeries) Ceiling() float64 {
	if s.Max > 0 {
		return s.Max
	}
	return 100
}

// Elevated reports whether the reading is high enough to colour.
func (s SparkSeries) Elevated() bool {
	warn := s.WarnAt
	if warn <= 0 {
		warn = s.Ceiling() * 0.75
	}
	return s.Value >= warn
}

// ValueLabel is the current reading for display.
func (s SparkSeries) ValueLabel() string {
	return strconv.FormatFloat(s.Value, 'f', 0, 64) + s.Unit
}

// HasMarker reports whether a reference line should be drawn.
func (s SparkSeries) HasMarker() bool {
	return s.Marker > 0 && s.Marker < s.Ceiling()
}

// MarkerY is the reference line's y coordinate in the viewBox.
func (s SparkSeries) MarkerY() string { return sparkY(s.Marker, s.Ceiling()) }

// Polyline maps the history onto the viewBox as "x,y x,y …".
func (s SparkSeries) Polyline() string { return sparkPolyline(s.Points, s.Ceiling()) }

// Polygon is Polyline closed down to the baseline, for the fill under it.
func (s SparkSeries) Polygon() string { return sparkPolygon(s.Points, s.Ceiling()) }

// The sparkline viewBox. Square and unitless: the SVG is stretched to whatever
// the card gives it with preserveAspectRatio="none", and the stroke is held at
// one device pixel with vector-effect so the distortion never shows.
const (
	sparkWidth  = 100.0
	sparkHeight = 100.0
)

// sparkPolyline maps readings onto the viewBox as "x,y x,y …".
//
// y is inverted (zero at the bottom) and the domain is fixed: 0 is the floor of
// the box and max its ceiling, whatever the data does. That fixed domain is the
// point of this function.
func sparkPolyline(values []float64, max float64) string {
	if len(values) == 0 {
		return ""
	}
	// A lone reading has no run to draw, so it becomes a flat line across the
	// box rather than a single invisible point.
	if len(values) == 1 {
		y := sparkY(values[0], max)
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
		sb.WriteString(sparkY(v, max))
	}
	return sb.String()
}

// sparkPolygon is sparkPolyline closed down to the baseline.
func sparkPolygon(values []float64, max float64) string {
	line := sparkPolyline(values, max)
	if line == "" {
		return ""
	}
	base := trim(sparkHeight)
	return "0," + base + " " + line + " " + trim(sparkWidth) + "," + base
}

// sparkY maps a reading to a y coordinate, clamped to the box.
func sparkY(v, max float64) string {
	if max <= 0 {
		max = 100
	}
	return trim(sparkHeight - clampTo(v, max)/max*sparkHeight)
}

// clampTo keeps a reading inside the fixed domain. The collectors clamp too,
// but this function is what stops a bad number drawing outside the box, and it
// is cheap enough to not make that someone else's job.
func clampTo(v, max float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > max:
		return max
	default:
		return v
	}
}

// trim formats a coordinate rounded to two decimals with no trailing zeros.
//
// The rounding is not cosmetic. A 180-point trace divides the width by 179, so
// the shortest round-tripping form of each x is seventeen digits, and several
// traces of those would add tens of kilobytes to a fragment that re-renders
// every ten seconds. Two decimals is a hundredth of the viewBox — far finer
// than the drawer can display.
func trim(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}
