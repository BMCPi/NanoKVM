package bmcsensor

import (
	"os"
	"testing"
	"time"
)

// sampler_test.go covers the one continuously-polling reader.
//
// Two things it exists for. First, a history: the record carries one instant,
// and the drawer draws a trend, so something has to keep the instants. Second,
// correct staleness for every consumer — a Reader measures it by watching a
// sequence number move between *its own* reads, so a cold consumer that reads
// once an hour observes a "new" sequence and calls a dead host's last sample
// live. One reader that never stops looking cannot make that mistake.

func newTestSampler(t *testing.T, path string, staleAfter time.Duration) (*Sampler, func(time.Time)) {
	t.Helper()
	s := NewSampler(NewReaderAt(path, staleAfter))
	now := time.Unix(1_700_000_000, 0)
	s.reader.now = func() time.Time { return now }
	s.now = func() time.Time { return now }
	return s, func(at time.Time) { now = at }
}

// Nothing has sampled yet — a consumer must still get a reading rather than an
// empty struct, or the first Redfish GET after boot answers with nothing.
func TestReadFallsBackToASynchronousReadBeforeTheFirstSample(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(3, 47000, 100, StatusTempValid|StatusI2CReady))
	s, _ := newTestSampler(t, path, DefaultStaleAfter)

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read before any sample: %v", err)
	}
	if got.Seq != 3 || got.SoCTempMilliC != 47000 {
		t.Errorf("reading = %+v", got.Record)
	}
}

// The bug this whole type exists for. A consumer holding its own Reader sets
// lastSeqAt on its first read, so it always sees the sample as fresh no matter
// how long the host has been silent.
func TestStalenessIsMeasuredFromTheFirstObservationNotTheFirstRead(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(9, 44000, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, 30*time.Second)
	base := time.Unix(1_700_000_000, 0)

	// The sampler observes the sequence now...
	if _, err := s.Sample(); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	// ...the host then goes quiet, and a consumer reads much later.
	setNow(base.Add(5 * time.Minute))

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.Stale {
		t.Error("a sample whose sequence stopped five minutes ago read as live; " +
			"staleness must be measured from when the sequence was first seen")
	}
}

// Staleness has to be recomputed when the reading is handed out, not frozen at
// sample time — otherwise a cached sample says "live" for a whole interval
// after it stopped being so.
func TestReadRecomputesStalenessOnEveryCall(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(4, 45000, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, 30*time.Second)
	base := time.Unix(1_700_000_000, 0)

	if _, err := s.Sample(); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got, _ := s.Read(); got.Stale {
		t.Fatal("fresh sample read as stale")
	}

	setNow(base.Add(45 * time.Second))
	if got, _ := s.Read(); !got.Stale {
		t.Error("the cached sample still reads as live 45s past a 30s staleness " +
			"window; Stale was frozen at sample time")
	}
}

// The history is what the graphs draw.
func TestSampleRecordsHistory(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(1, 46500, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, DefaultStaleAfter)
	base := time.Unix(1_700_000_000, 0)

	s.Sample()
	// A new sequence, or the next sample is the same instant and goes nowhere.
	writeRecord(t, path, buildRecord(2, 52000, 70, StatusTempValid))
	setNow(base.Add(10 * time.Second))
	s.Sample()

	got := s.History()
	if len(got) != 2 {
		t.Fatalf("history = %d points, want 2", len(got))
	}
	if got[0].TempC != 46.5 || got[1].TempC != 52 {
		t.Errorf("temperatures = %v, %v; want 46.5, 52", got[0].TempC, got[1].TempC)
	}
	// buildRecord's fan block is level 2 of 4 at 49% duty.
	if !got[1].FanValid || got[1].FanDutyPct != 49 {
		t.Errorf("fan = %+v, want a valid 49%% duty", got[1])
	}
}

// A powered-off host leaves its last record in the EEPROM, parsing perfectly.
// Extending the trace with it would draw a die temperature for a machine that
// is switched off — the same live-looking lie the Redfish view refuses to tell.
func TestAStaleSampleDoesNotExtendTheTrace(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(7, 48000, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, 30*time.Second)
	base := time.Unix(1_700_000_000, 0)

	s.Sample()
	before := len(s.History())

	// The host stops pushing: same sequence, much later.
	setNow(base.Add(5 * time.Minute))
	s.Sample()

	if got := len(s.History()); got != before {
		t.Errorf("history grew to %d while the host was silent (was %d); a stale "+
			"sample must not extend the trace", got, before)
	}
}

// A temperature the pTA could not actually read is carried forward inside the
// record, so it is not a reading and must not be plotted as one.
func TestAnInvalidTemperatureIsNotPlotted(t *testing.T) {
	// StatusTempValid absent.
	path := fakeEEPROM(t, buildRecord(1, 46500, 60, StatusI2CReady))
	s, setNow := newTestSampler(t, path, DefaultStaleAfter)
	base := time.Unix(1_700_000_000, 0)

	s.Sample()
	writeRecord(t, path, buildRecord(2, 46500, 70, StatusI2CReady))
	setNow(base.Add(10 * time.Second))
	s.Sample()

	got := s.History()
	if len(got) != 2 {
		t.Fatalf("history = %d points, want 2", len(got))
	}
	for i, p := range got {
		if p.TempValid {
			t.Errorf("point %d: a record without StatusTempValid was recorded as a "+
				"valid reading", i)
		}
	}
}

func TestHistoryIsBounded(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(1, 46500, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, time.Hour)
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < sensorDepth*2; i++ {
		writeRecord(t, path, buildRecord(uint32(i+1), 46500, 60, StatusTempValid))
		setNow(base.Add(time.Duration(i) * 10 * time.Second))
		s.Sample()
	}
	if n := len(s.History()); n > sensorDepth {
		t.Errorf("history grew to %d, want at most %d", n, sensorDepth)
	}
}

func TestHistoryHandsOutACopy(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(1, 46500, 60, StatusTempValid))
	s, setNow := newTestSampler(t, path, DefaultStaleAfter)
	s.Sample()
	writeRecord(t, path, buildRecord(2, 47000, 70, StatusTempValid))
	setNow(time.Unix(1_700_000_000, 0).Add(10 * time.Second))
	s.Sample()

	got := s.History()
	if len(got) == 0 {
		t.Fatal("no history")
	}
	got[0].TempC = 999
	if again := s.History(); again[0].TempC == 999 {
		t.Error("mutating the returned slice changed the stored history")
	}
}

// An absent EEPROM is a board without the slave device, not a failure.
func TestAvailableIsForwarded(t *testing.T) {
	s, _ := newTestSampler(t, fakeEEPROM(t, buildRecord(1, 46500, 60, StatusTempValid)), time.Minute)
	if !s.Available() {
		t.Error("Available() = false for an existing attribute")
	}
	absent, _ := newTestSampler(t, t.TempDir()+"/nope", time.Minute)
	if absent.Available() {
		t.Error("Available() = true for a missing attribute")
	}
}

// writeRecord replaces the record in an existing fixture, leaving the rest of
// the emulated part alone.
func writeRecord(t *testing.T, path string, record []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(record, RecordOffset); err != nil {
		t.Fatal(err)
	}
}
