package ring

import "testing"

// ring_test.go pins the bounded history both metric samplers store through.
//
// It exists because the same twenty lines were written twice — once for the
// BMC's own resource history, once for the host's sensor record — and the
// second copy shipped with a defect the first did not have. A ring is small
// enough that duplicating it feels cheaper than sharing it, and exactly big
// enough to get wrong in a way nobody notices for a month.

func TestRingKeepsTheMostRecentValues(t *testing.T) {
	r := NewRing[int](3)
	for i := 1; i <= 5; i++ {
		r.Append(i)
	}
	got := r.Snapshot()
	if len(got) != 3 {
		t.Fatalf("Snapshot() = %v, want 3 values", got)
	}
	// Oldest first, and the two oldest are gone.
	for i, want := range []int{3, 4, 5} {
		if got[i] != want {
			t.Errorf("Snapshot()[%d] = %d, want %d", i, got[i], want)
		}
	}
}

// The defect the shared version exists to prevent: a full ring must still hand
// back every value it holds. An earlier hand-rolled copy dropped its oldest
// entry on every read, forever, to compensate for one unwanted first sample.
func TestAFullRingReturnsEverythingItHolds(t *testing.T) {
	r := NewRing[int](4)
	for i := 0; i < 40; i++ {
		r.Append(i)
	}
	if got := len(r.Snapshot()); got != 4 {
		t.Errorf("a full ring returned %d values, want its full 4", got)
	}
	if got := r.Len(); got != 4 {
		t.Errorf("Len() = %d, want 4", got)
	}
}

// Callers range over the snapshot while the sampler keeps appending, so it has
// to be a copy.
func TestSnapshotIsACopy(t *testing.T) {
	r := NewRing[int](3)
	r.Append(1)
	r.Append(2)

	got := r.Snapshot()
	got[0] = 999
	if again := r.Snapshot(); again[0] != 1 {
		t.Errorf("mutating a snapshot changed the ring: %v", again)
	}

	// Appending after a snapshot must not write into the slice already handed
	// out — the bug a shared backing array would cause.
	before := r.Snapshot()
	r.Append(3)
	if before[len(before)-1] != 2 {
		t.Errorf("appending mutated a snapshot taken earlier: %v", before)
	}
}

// Carry-forward needs the previous value, and it has to be the one the ring
// will actually report as last.
func TestLastReportsTheNewestValue(t *testing.T) {
	r := NewRing[int](3)
	if _, ok := r.Last(); ok {
		t.Error("Last() on an empty ring reported a value")
	}
	r.Append(7)
	r.Append(8)
	if got, ok := r.Last(); !ok || got != 8 {
		t.Errorf("Last() = %d, %v; want 8, true", got, ok)
	}
}

func TestAnEmptyRingSnapshotsToNothing(t *testing.T) {
	r := NewRing[int](3)
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() = %v, want empty", got)
	}
}

// A zero or negative bound would otherwise mean either a ring that discards
// everything or one that grows without limit on a 256 MB device.
func TestANonPositiveBoundStillBounds(t *testing.T) {
	r := NewRing[int](0)
	for i := 0; i < 10; i++ {
		r.Append(i)
	}
	if n := r.Len(); n == 0 || n > 1 {
		t.Errorf("Len() = %d for a zero-bound ring, want exactly 1", n)
	}
}
