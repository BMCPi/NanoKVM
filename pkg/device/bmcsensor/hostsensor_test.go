package bmcsensor

// hostsensor_test.go is the conformance suite for this package's
// implementation of pkg/device/hostsensor.Source (and Trend): does the
// Sampler actually behave the way a board-agnostic consumer expects, not just
// the way this package's own Read()/History() tests expect.

import (
	"testing"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/device/hostsensor"
)

// A board with no host EEPROM attribute reads as ErrNoRecord, the ordinary
// pre-boot state (see reader_test.go); as a hostsensor.Source that must come
// across as "no telemetry available", not a read error a consumer has to
// interpret.
func TestLatestReportsAbsenceBeforeAnyRecord(t *testing.T) {
	s, _ := newTestSampler(t, fakeEEPROM(t, nil), DefaultStaleAfter) // all zeroes
	if reading, ok := s.Latest(); ok {
		t.Errorf("Latest() = %+v, true before any record; want ok=false", reading)
	}
}

// A corrupted record (a bad CRC, say) must read the same way as no record at
// all through this seam: hostsensor.Source carries no error, so every read
// failure is "no telemetry" to a consumer.
func TestLatestReportsAbsenceOnAnUnparseableRecord(t *testing.T) {
	bad := buildRecord(1, 46500, 60, StatusTempValid)
	bad[10] ^= 0xFF // corrupt a byte inside the CRC's coverage
	s, _ := newTestSampler(t, fakeEEPROM(t, bad), DefaultStaleAfter)
	if reading, ok := s.Latest(); ok {
		t.Errorf("Latest() = %+v, true on a corrupt record; want ok=false", reading)
	}
}

// The values a consumer actually reads through the seam, not just whether
// Latest reports one.
func TestLatestCarriesTheRecordThroughToHostReading(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(1, 52300, 60, StatusTempValid))
	s, _ := newTestSampler(t, path, DefaultStaleAfter)

	reading, ok := s.Latest()
	if !ok {
		t.Fatal("Latest() ok = false for a valid record")
	}
	if !reading.TempValid || reading.TempC != 52.3 {
		t.Errorf("TempC/TempValid = %v/%v, want 52.3/true", reading.TempC, reading.TempValid)
	}
	// buildRecord's fan block is level 2 of 4, 49% duty, no tach.
	if !reading.FanValid || reading.FanDutyPct != 49 || reading.FanLevel != 2 || reading.FanMaxLevel != 4 {
		t.Errorf("fan fields = %+v, want valid 49%% at level 2/4", reading)
	}
	if reading.FanRPMValid {
		t.Error("FanRPMValid = true for a record with no tach capture (rpm=0)")
	}
	if reading.ThrottleValid {
		t.Error("ThrottleValid = true for a v2-shaped record with no throttle word")
	}
}

// The seam's whole reason to exist: a host actively throttling must come
// through as live Conditions a board-agnostic consumer can render without
// knowing this package's bit layout.
func TestLatestCarriesLiveThrottleConditions(t *testing.T) {
	rec := setThrottle(buildRecord(1, 80000, 60, StatusTempValid), ThrottleThrottled|ThrottleSoftTempLimit)
	s, _ := newTestSampler(t, fakeEEPROM(t, rec), DefaultStaleAfter)

	reading, ok := s.Latest()
	if !ok {
		t.Fatal("Latest() ok = false for a valid record")
	}
	if !reading.ThrottleValid {
		t.Fatal("ThrottleValid = false for a record carrying the throttle word")
	}
	if !reading.Condition(hostsensor.ConditionThrottled) || !reading.Condition(hostsensor.ConditionSoftTempLimit) {
		t.Errorf("Conditions = %v, want Throttled and SoftTempLimit", reading.Conditions)
	}
	if reading.Condition(hostsensor.ConditionUnderVoltage) {
		t.Errorf("Conditions = %v, want no UnderVoltage", reading.Conditions)
	}
}

// A stale sample (the host went quiet) must still come back as a reading —
// consumers decide what staleness means to them — just flagged as such.
func TestLatestSurfacesStaleness(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(9, 44000, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, 30*time.Second)
	base := time.Unix(1_700_000_000, 0)

	if _, err := s.Sample(); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	setNow(base.Add(5 * time.Minute))

	reading, ok := s.Latest()
	if !ok {
		t.Fatal("Latest() ok = false for a still-parseable, merely stale record")
	}
	if !reading.Stale {
		t.Error("Stale = false five minutes after the sequence stopped moving")
	}
}

// This Source's own thresholds, the RPi numbers the UI used to hardcode.
func TestThresholdsAreTheRPiDomain(t *testing.T) {
	got := NewSampler(NewReader()).Thresholds()
	want := hostsensor.Thresholds{TempCeilingC: 100, TempWarnC: 80}
	if got != want {
		t.Errorf("Thresholds() = %+v, want %+v", got, want)
	}
}

// The overview card's trend graph draws Trend()'s points, not History()'s
// directly — this is the seam between them.
func TestTrendMirrorsHistory(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(1, 46500, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, DefaultStaleAfter)
	base := time.Unix(1_700_000_000, 0)

	s.Sample()
	writeRecord(t, path, buildRecord(2, 52000, 70, StatusTempValid))
	setNow(base.Add(10 * time.Second))
	s.Sample()

	got := s.Trend()
	if len(got) != 2 {
		t.Fatalf("Trend() = %d points, want 2", len(got))
	}
	if got[0].TempC != 46.5 || got[1].TempC != 52 {
		t.Errorf("Trend TempC = %v, %v; want 46.5, 52", got[0].TempC, got[1].TempC)
	}
	if !got[1].FanValid || got[1].FanDutyPct != 49 {
		t.Errorf("Trend fan = %+v, want a valid 49%% duty", got[1])
	}
}

// Before anything has sampled, there is no trend to draw — a UI must fall
// back to "not sampling yet" rather than plot an empty line.
func TestTrendIsEmptyBeforeAnyHistory(t *testing.T) {
	s := NewSampler(NewReaderAt(t.TempDir()+"/nope", DefaultStaleAfter))
	if got := s.Trend(); got != nil {
		t.Errorf("Trend() = %v before any Sample; want nil", got)
	}
}
