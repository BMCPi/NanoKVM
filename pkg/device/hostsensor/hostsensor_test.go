package hostsensor

import "testing"

// hostsensor_test.go covers the registry and the Reading helper this package
// owns directly. Board-specific Source conformance (does bmcsensor's Sampler
// actually behave like a Source) lives in pkg/device/bmcsensor, next to the
// implementation it is testing.

// fakeSource is the minimal Source a registry test needs; it carries no
// telemetry logic of its own.
type fakeSource struct {
	reading Reading
	ok      bool
}

func (f fakeSource) Latest() (Reading, bool) { return f.reading, f.ok }
func (f fakeSource) Thresholds() Thresholds  { return Thresholds{TempCeilingC: 100, TempWarnC: 80} }

// A process that never registers a Source — the NUC path this seam exists
// for — must report absence rather than a zero-value Source a caller could
// mistake for a real one.
func TestGetReportsAbsenceBeforeAnyRegister(t *testing.T) {
	Register(nil) // in case an earlier test in this binary left one registered
	if s, ok := Get(); ok || s != nil {
		t.Errorf("Get() = %v, %v before any Register; want nil, false", s, ok)
	}
}

func TestRegisterThenGetReturnsTheSameSource(t *testing.T) {
	defer Register(nil)

	want := fakeSource{reading: Reading{TempC: 42, TempValid: true}, ok: true}
	Register(want)

	got, ok := Get()
	if !ok {
		t.Fatal("Get() ok = false after Register")
	}
	// Reading carries a slice field, so Source values are not comparable
	// with == here; check what the registry is actually for instead — that
	// Get hands back a Source whose Latest() is the one Registered.
	reading, latestOK := got.Latest()
	if !latestOK || reading.TempC != want.reading.TempC {
		t.Errorf("Get().Latest() = %+v, %v, want %+v, true", reading, latestOK, want.reading)
	}
}

// A later Register must overwrite, not merge with, an earlier one — main
// only ever calls this once in practice, but nothing here should assume that.
func TestRegisterOverwritesAPriorSource(t *testing.T) {
	defer Register(nil)

	Register(fakeSource{reading: Reading{TempC: 1}, ok: true})
	second := fakeSource{reading: Reading{TempC: 2}, ok: true}
	Register(second)

	got, ok := Get()
	if !ok {
		t.Fatal("Get() ok = false after Register")
	}
	if reading, _ := got.Latest(); reading.TempC != 2 {
		t.Errorf("Get().Latest().TempC = %v, want 2 (the second Register's source)", reading.TempC)
	}
}

// Register(nil) is how a test restores the "no Source" state a later test in
// the same binary depends on, since the registry is process-wide.
func TestRegisterNilClearsTheRegistry(t *testing.T) {
	Register(fakeSource{reading: Reading{TempC: 1}, ok: true})
	Register(nil)

	if s, ok := Get(); ok || s != nil {
		t.Errorf("Get() = %v, %v after Register(nil); want nil, false", s, ok)
	}
}

func TestReadingConditionMembership(t *testing.T) {
	r := Reading{ThrottleValid: true, Conditions: []Condition{ConditionThrottled, ConditionUnderVoltage}}

	if !r.Condition(ConditionThrottled) {
		t.Error("Condition(ConditionThrottled) = false, want true")
	}
	if !r.Condition(ConditionUnderVoltage) {
		t.Error("Condition(ConditionUnderVoltage) = false, want true")
	}
	if r.Condition(ConditionFreqCapped) {
		t.Error("Condition(ConditionFreqCapped) = true, want false: not in the list")
	}
}

// nil Conditions (a Source that never sent the power-health word at all)
// must not panic and must report every condition absent.
func TestReadingConditionOnANilList(t *testing.T) {
	var r Reading
	if r.Condition(ConditionThrottled) {
		t.Error("Condition() on a zero-value Reading reported a live condition")
	}
}
