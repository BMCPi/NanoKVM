package components

import (
	"context"
	"strings"
	"testing"
)

// overview_host_sensors_test.go covers the card that reports on the *managed
// host*, not on the BMC.
//
// Its states are the point. The record arrives over I2C from a machine that
// can be switched off, and a powered-off host leaves its last reading in the
// EEPROM parsing perfectly — so the difference between "45 °C" and "45 °C, but
// the host stopped talking twenty minutes ago" is the difference between a
// useful card and a lie. Each state below is one way of getting that wrong.

func renderHostSensors(t *testing.T, m OverviewHostSensors) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return OverviewHostSensorsBody(m).Render(context.Background(), w)
	})
}

func liveHostSensors() OverviewHostSensors {
	return OverviewHostSensors{
		Available: true, Reporting: true, Sampling: true, ThrottleKnown: true,
		Temperature: SparkSeries{
			Label: "SoC temperature", Value: 52, Unit: "°C", Valid: true,
			Points: []float64{50, 52}, Max: hostTempCeiling,
			WarnAt: hostTempThrottle, Marker: hostTempThrottle,
		},
		Fan: SparkSeries{
			Label: "Active cooler", Value: 49, Unit: "%", Valid: true,
			Points: []float64{45, 49}, Detail: "level 2/4",
		},
	}
}

// A board without the slave EEPROM has no channel to the host at all, which is
// a different thing from a host that has not booted yet. Conflating them sends
// an operator looking for a fault in the wrong machine.
func TestNoSensorChannelIsDistinctFromNoHost(t *testing.T) {
	noBoard := renderHostSensors(t, OverviewHostSensors{})
	if !strings.Contains(noBoard, "no sensor channel") {
		t.Errorf("a board with no EEPROM does not say so: %s", noBoard)
	}

	waiting := renderHostSensors(t, OverviewHostSensors{Available: true})
	if strings.Contains(waiting, "no sensor channel") {
		t.Error("a working board with no host reading reads as a hardware fault")
	}
	if !strings.Contains(waiting, "Waiting for the host") {
		t.Errorf("the waiting state does not say what it is waiting for: %s", waiting)
	}
}

// The reading this card must never let stand unqualified: a die temperature
// for a machine that is switched off.
func TestAQuietHostSaysSoBesideItsFrozenTrace(t *testing.T) {
	m := liveHostSensors()
	m.Reporting = false
	html := renderHostSensors(t, m)

	if !strings.Contains(html, "stopped reporting") {
		t.Error("a stale reading renders with nothing to say it is stale; the " +
			"temperature reads as current for a host that may be powered off")
	}
	// The trace still draws — it is what happened before the host went quiet.
	if !strings.Contains(html, "<svg") {
		t.Error("the frozen trace was dropped; it is still the last real data")
	}
}

func TestALiveHostDoesNotClaimToBeQuiet(t *testing.T) {
	if html := renderHostSensors(t, liveHostSensors()); strings.Contains(html, "stopped reporting") {
		t.Error("a reporting host is labelled as quiet")
	}
}

// Temperature is not a percentage. It gets its own ceiling, and the throttle
// point is drawn on the trace so a reading means something against it.
func TestTemperatureIsDrawnAgainstTheThrottlePoint(t *testing.T) {
	html := renderHostSensors(t, liveHostSensors())

	if !strings.Contains(html, "stroke-dasharray") {
		t.Error("no reference marker: the trace has nothing to be read against")
	}
	// 80 of 100 inverts to y=20.
	if !strings.Contains(html, `y1="20"`) {
		t.Errorf("the marker is not at the throttle point: %s", html)
	}
	if !strings.Contains(html, "52°C") {
		t.Error("the temperature reading is missing its unit")
	}
}

// A host below the throttle point is not in trouble, and one above it is. The
// colour is the only thing carrying that at a glance.
func TestTheTemperatureColoursAtTheThrottlePointNotAtThreeQuarters(t *testing.T) {
	warm := SparkSeries{Value: 78, Max: hostTempCeiling, WarnAt: hostTempThrottle}
	if warm.Elevated() {
		t.Error("78 °C is below the throttle point and should not be coloured; " +
			"a percentage's 75% threshold does not apply to a temperature")
	}
	hot := SparkSeries{Value: 84, Max: hostTempCeiling, WarnAt: hostTempThrottle}
	if !hot.Elevated() {
		t.Error("84 °C is past the throttle point and should be coloured")
	}
}

// Three different throttle states, three different things to say. The middle
// one is the trap: a firmware too old to report the word must not read as
// "nothing wrong".
func TestThrottleStatesAreDistinguishable(t *testing.T) {
	nominal := renderHostSensors(t, liveHostSensors())
	if !strings.Contains(nominal, "nominal") {
		t.Error("a healthy host does not say so")
	}

	unknown := liveHostSensors()
	unknown.ThrottleKnown = false
	html := renderHostSensors(t, unknown)
	if strings.Contains(html, "nominal") {
		t.Error("a firmware that does not report power health reads as healthy")
	}
	if !strings.Contains(html, "not reported") {
		t.Errorf("the unknown state does not say it is unknown: %s", html)
	}

	throttled := liveHostSensors()
	throttled.Throttles = []string{"Under-voltage", "Throttled"}
	html = renderHostSensors(t, throttled)
	if strings.Contains(html, "nominal") {
		t.Error("a throttled host still claims to be nominal")
	}
	for _, want := range []string{"Under-voltage", "Throttled"} {
		if !strings.Contains(html, want) {
			t.Errorf("%q is missing from the badges", want)
		}
	}
}

// A version-1 record carries no fan block. Drawing the fan flat at zero would
// report a stopped cooler on a host whose fan is running fine.
func TestAHostWithNoFanBlockDrawsNoFanTrace(t *testing.T) {
	m := liveHostSensors()
	m.Fan = SparkSeries{Label: "Active cooler"} // not valid
	html := renderHostSensors(t, m)

	if strings.Contains(html, "Active cooler") {
		t.Error("a host that reports no fan rendered a fan trace, which reads " +
			"as a cooler stopped at 0%")
	}
	if n := strings.Count(html, "<svg"); n != 1 {
		t.Errorf("%d traces drawn, want only the temperature", n)
	}
}

func TestHealthyNeedsAReportingHostAndAKnownWord(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*OverviewHostSensors)
		want bool
	}{
		{"live and clear", func(*OverviewHostSensors) {}, true},
		{"quiet", func(m *OverviewHostSensors) { m.Reporting = false }, false},
		{"word unknown", func(m *OverviewHostSensors) { m.ThrottleKnown = false }, false},
		{"throttled", func(m *OverviewHostSensors) { m.Throttles = []string{"Throttled"} }, false},
	} {
		m := liveHostSensors()
		tc.mut(&m)
		if got := m.Healthy(); got != tc.want {
			t.Errorf("%s: Healthy() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
