package fragments

// overview_test.go covers the Resources card's model.
//
// resourceDetail is the figure that gives a percentage meaning, and it has one
// rule worth pinning: both halves must be in the same unit. "900 MB / 6.8 GB"
// is arithmetic the card exists to save the reader from, and the unit is
// chosen from the total precisely so the pair can never be mixed.

import (
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/device/hostsensor"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/sysinfo"
)

func TestResourceDetailKeepsBothHalvesInOneUnit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		used, total uint64
		want        string
	}{
		{"this device's memory", 161, 246, "161 / 246 MB"},
		{"just under the GB switch", 900, 1023, "900 / 1023 MB"},
		// The moment the total reaches a GB, so does the used half — the bug
		// this guards is "900 MB / 1.0 GB".
		{"at the GB switch", 900, 1024, "0.9 / 1.0 GB"},
		{"a data volume", 1229, 6963, "1.2 / 6.8 GB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceDetail(tc.used, tc.total); got != tc.want {
				t.Errorf("resourceDetail(%d, %d) = %q, want %q", tc.used, tc.total, got, tc.want)
			}
		})
	}
}

// A subsystem with no total has no absolute figure to show, and "0 / 0 MB"
// beside a percentage is worse than nothing.
func TestResourceDetailIsEmptyWithoutATotal(t *testing.T) {
	if got := resourceDetail(0, 0); got != "" {
		t.Errorf("resourceDetail(0, 0) = %q, want empty", got)
	}
}

// With no history the card must report that it is still collecting rather than
// rendering three empty plots, which read as measured zeros.
func TestResourcesModelIsNotSamplingBeforeAnyHistory(t *testing.T) {
	if len(sysinfo.ResourceHistory()) > 0 {
		t.Skip("the sampler is running in this process; the empty case cannot be observed")
	}
	if m := overviewResourcesModel(); m.Sampling {
		t.Error("the model claims to be sampling with no history behind it")
	}
}

// The drawer's copy for the host's power-health conditions. The registered
// Source decides which are live (pkg/device/hostsensor); this decides how
// they read.
func TestThrottleLabelsRenderEveryKnownCondition(t *testing.T) {
	all := []hostsensor.Condition{
		hostsensor.ConditionUnderVoltage,
		hostsensor.ConditionThrottled,
		hostsensor.ConditionFreqCapped,
		hostsensor.ConditionSoftTempLimit,
	}
	got := throttleLabels(all)
	want := []string{"Under-voltage", "Throttled", "Frequency capped", "Soft temperature limit"}
	if len(got) != len(want) {
		t.Fatalf("throttleLabels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("label %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A condition a Source grows and this map has not must still surface. A live
// fault dropped on the floor is the worst available outcome.
func TestAnUnmappedConditionStillShows(t *testing.T) {
	got := throttleLabels([]hostsensor.Condition{"SomethingNew"})
	if len(got) != 1 || got[0] != "SomethingNew" {
		t.Errorf("throttleLabels = %v; an unknown condition must not vanish", got)
	}
}

func TestNoConditionsIsNoLabels(t *testing.T) {
	if got := throttleLabels(nil); got != nil {
		t.Errorf("throttleLabels(nil) = %v, want nil", got)
	}
}

// fakeHostSensorModelSource is a minimal hostsensor.Source for
// overviewHostSensorsModel's own tests; it deliberately does not implement
// hostsensor.Trend, so a registered Source with no history support is
// covered too.
type fakeHostSensorModelSource struct {
	reading    hostsensor.Reading
	sampled    bool
	thresholds hostsensor.Thresholds
}

func (f fakeHostSensorModelSource) Latest() (hostsensor.Reading, bool) {
	return f.reading, f.sampled
}
func (f fakeHostSensorModelSource) Thresholds() hostsensor.Thresholds { return f.thresholds }

// The NUC path this seam exists for: no registered Source at all must render
// as "no sensor channel", not a panic or a zero-value reading that could be
// mistaken for a real one.
func TestOverviewHostSensorsModelWithNoSourceReportsAbsence(t *testing.T) {
	hostsensor.Register(nil)

	m := overviewHostSensorsModel()
	if m.Available {
		t.Error("Available = true with no hostsensor.Source registered")
	}
	if m.Reporting || m.Sampling {
		t.Errorf("m = %+v, want Reporting and Sampling both false with no Source", m)
	}
}

// A registered Source that has not produced a reading yet (the ordinary state
// before the host boots past its firmware) is available but not yet sampling
// — distinct from "no channel at all".
func TestOverviewHostSensorsModelWithSourceButNoReadingIsWaiting(t *testing.T) {
	defer hostsensor.Register(nil)
	hostsensor.Register(fakeHostSensorModelSource{sampled: false})

	m := overviewHostSensorsModel()
	if !m.Available {
		t.Error("Available = false with a Source registered")
	}
	if m.Reporting || m.Sampling {
		t.Errorf("m = %+v, want Reporting and Sampling both false before any reading", m)
	}
}

// A registered Source that does not implement the optional Trend extension
// must still report a live reading — it just never gets to "Sampling", since
// there is no history to plot.
func TestOverviewHostSensorsModelWithoutTrendStillReportsLive(t *testing.T) {
	defer hostsensor.Register(nil)
	hostsensor.Register(fakeHostSensorModelSource{
		sampled:    true,
		reading:    hostsensor.Reading{TempC: 47, TempValid: true},
		thresholds: hostsensor.Thresholds{TempCeilingC: 100, TempWarnC: 80},
	})

	m := overviewHostSensorsModel()
	if !m.Available {
		t.Fatal("Available = false with a Source registered")
	}
	if !m.Reporting {
		t.Error("Reporting = false for a live, non-stale reading")
	}
	if m.Sampling {
		t.Error("Sampling = true for a Source with no Trend to draw from")
	}
}

// fakeHostSensorTrendSource additionally implements hostsensor.Trend, so the
// model's optional history path can be exercised.
type fakeHostSensorTrendSource struct {
	fakeHostSensorModelSource
	trend []hostsensor.Reading
}

func (f fakeHostSensorTrendSource) Trend() []hostsensor.Reading { return f.trend }

// The values a Source's Thresholds supplies must reach the drawn series
// directly — this is the seam that replaced the UI's own hardcoded 100/80.
func TestOverviewHostSensorsModelUsesTheSourcesThresholds(t *testing.T) {
	defer hostsensor.Register(nil)
	reading := hostsensor.Reading{TempC: 61, TempValid: true, FanDutyPct: 40, FanValid: true}
	hostsensor.Register(fakeHostSensorTrendSource{
		fakeHostSensorModelSource: fakeHostSensorModelSource{
			sampled:    true,
			reading:    reading,
			thresholds: hostsensor.Thresholds{TempCeilingC: 90, TempWarnC: 70},
		},
		trend: []hostsensor.Reading{reading, reading},
	})

	m := overviewHostSensorsModel()
	if !m.Sampling {
		t.Fatal("Sampling = false with a Trend-capable Source and two points")
	}
	if m.Temperature.Max != 90 || m.Temperature.WarnAt != 70 || m.Temperature.Marker != 70 {
		t.Errorf("temperature domain = max %v warnAt %v marker %v, want the registered Source's 90/70",
			m.Temperature.Max, m.Temperature.WarnAt, m.Temperature.Marker)
	}
}
