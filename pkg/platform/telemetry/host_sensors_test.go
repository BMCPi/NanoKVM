package telemetry

// host_sensors_test.go covers sampleHostSensors: what the host-sensor gauges
// observe with and without a registered hostsensor.Source. It stays off the
// OTel/Prometheus collection pipeline entirely — sampleHostSensors is the
// seam RegisterCallback's closure delegates to, and testing it directly is
// both simpler and immune to the process-global telemetry Init() this
// package's other tests already share.

import (
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/device/hostsensor"
)

type fakeHostSource struct {
	reading hostsensor.Reading
	ok      bool
}

func (f fakeHostSource) Latest() (hostsensor.Reading, bool) { return f.reading, f.ok }
func (f fakeHostSource) Thresholds() hostsensor.Thresholds {
	return hostsensor.Thresholds{TempCeilingC: 100, TempWarnC: 80}
}

// The NUC path this seam exists for: no Source registered at all, so every
// gauge but the one that is always exported must stay unset rather than
// carry a fabricated reading.
func TestSampleHostSensorsWithNoSourceReportsOnlyAbsence(t *testing.T) {
	hostsensor.Register(nil)

	got := sampleHostSensors()
	if got.reporting != 0 {
		t.Errorf("reporting = %v, want 0 with no Source registered", got.reporting)
	}
	if got.hasTemp || got.hasFan || got.hasThrottle {
		t.Errorf("sample = %+v, want nothing else set with no Source registered", got)
	}
}

// A registered Source that has never produced a reading (host not booted
// past its firmware yet) must read the same as no Source at all to a
// scraper: quiet, not broken.
func TestSampleHostSensorsWithNoReadingYetReportsAbsence(t *testing.T) {
	defer hostsensor.Register(nil)
	hostsensor.Register(fakeHostSource{ok: false})

	got := sampleHostSensors()
	if got.reporting != 0 || got.hasTemp || got.hasFan || got.hasThrottle {
		t.Errorf("sample = %+v, want a zero sample before any reading", got)
	}
}

// A stale reading (the host went quiet) must not extend the gauges either —
// exporting a frozen temperature would read as a live one to a scraper.
func TestSampleHostSensorsWithAStaleReadingReportsAbsence(t *testing.T) {
	defer hostsensor.Register(nil)
	hostsensor.Register(fakeHostSource{
		ok:      true,
		reading: hostsensor.Reading{Stale: true, TempC: 55, TempValid: true},
	})

	got := sampleHostSensors()
	if got.reporting != 0 || got.hasTemp {
		t.Errorf("sample = %+v, want reporting=0 and no temp for a stale reading", got)
	}
}

// The values a live reading carries through, including the throttle
// conditions rendered as four independent 0/1 gauges.
func TestSampleHostSensorsWithALiveReadingReportsEveryValue(t *testing.T) {
	defer hostsensor.Register(nil)
	hostsensor.Register(fakeHostSource{
		ok: true,
		reading: hostsensor.Reading{
			TempC: 52.3, TempValid: true,
			FanDutyPct: 61, FanValid: true, FanRPM: 1800, FanRPMValid: true,
			ThrottleValid: true,
			Conditions:    []hostsensor.Condition{hostsensor.ConditionUnderVoltage},
		},
	})

	got := sampleHostSensors()
	if got.reporting != 1 {
		t.Errorf("reporting = %v, want 1 for a live reading", got.reporting)
	}
	if !got.hasTemp || got.temp != 52.3 {
		t.Errorf("temp = %v/%v, want 52.3/true", got.temp, got.hasTemp)
	}
	if !got.hasFan || got.fanDuty != 61 {
		t.Errorf("fanDuty = %v/%v, want 61/true", got.fanDuty, got.hasFan)
	}
	if !got.hasFanRPM || got.fanRPM != 1800 {
		t.Errorf("fanRPM = %v/%v, want 1800/true", got.fanRPM, got.hasFanRPM)
	}
	if !got.hasThrottle {
		t.Fatal("hasThrottle = false for a reading with ThrottleValid=true")
	}
	if got.underVoltage != 1 {
		t.Errorf("underVoltage = %v, want 1 (the one live condition)", got.underVoltage)
	}
	if got.throttle != 0 || got.freqCapped != 0 || got.softTempLimit != 0 {
		t.Errorf("throttle=%v freqCapped=%v softTempLimit=%v, want all 0",
			got.throttle, got.freqCapped, got.softTempLimit)
	}
}

// A fan block with no tachometer capture must not fabricate an RPM of 0 —
// that reads as a stalled fan rather than one with no sensor for it.
func TestSampleHostSensorsOmitsFanRPMWhenNotCaptured(t *testing.T) {
	defer hostsensor.Register(nil)
	hostsensor.Register(fakeHostSource{
		ok:      true,
		reading: hostsensor.Reading{FanDutyPct: 40, FanValid: true, FanRPMValid: false},
	})

	got := sampleHostSensors()
	if !got.hasFan {
		t.Fatal("hasFan = false for a valid fan reading")
	}
	if got.hasFanRPM {
		t.Error("hasFanRPM = true for a reading with no tachometer capture")
	}
}
