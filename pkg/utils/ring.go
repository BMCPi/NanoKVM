package utils

// ring.go is the bounded history the metric samplers store through.
//
// Both of them — the BMC's own cpu/memory/disk in pkg/sysinfo and the host's
// sensor record in pkg/bmcsensor — need the same thing: keep the last N
// readings, drop the oldest, hand callers a copy they can range over while the
// sampler keeps writing. That is twenty lines, which is small enough that
// writing it twice feels cheaper than sharing it and exactly big enough to get
// wrong quietly. The second copy did: it discarded its oldest entry on every
// read, forever, to compensate for one unwanted first sample.
//
// Deliberately NOT internally synchronized. Both callers already hold a lock
// across a read-then-append — carry-forward needs the previous value and the
// append to be one atomic step — so an internal mutex would be a second lock
// that guards nothing extra while suggesting the caller needs none.

// Ring is a bounded FIFO of the most recent values, oldest first.
//
// The zero value is not usable; call NewRing. Not safe for concurrent use:
// callers hold their own lock (see the file comment).
type Ring[T any] struct {
	max int
	buf []T
}

// NewRing returns a ring holding at most capacity values. A non-positive
// capacity is raised to one rather than accepted: a ring that discards
// everything and a ring that grows without limit are both worse than the
// smallest real one, and this runs on a device with 256 MB of RAM.
func NewRing[T any](capacity int) Ring[T] {
	if capacity < 1 {
		capacity = 1
	}
	return Ring[T]{max: capacity}
}

// Append stores a value, evicting the oldest once the ring is full.
//
// The shift copies the backing array rather than advancing an index. For the
// sizes here — a couple of hundred small structs, every ten seconds — the copy
// is cheaper than the modular arithmetic a true circular buffer needs at every
// read, and it keeps Snapshot a plain slice copy.
func (r *Ring[T]) Append(v T) {
	r.buf = append(r.buf, v)
	if len(r.buf) > r.max {
		r.buf = append(r.buf[:0], r.buf[1:]...)
	}
}

// Snapshot returns a copy of the stored values, oldest first.
//
// A copy, not the backing slice: callers range over it while the sampler keeps
// appending, and Append's shift writes through the same array.
func (r *Ring[T]) Snapshot() []T {
	if len(r.buf) == 0 {
		return nil
	}
	return append([]T(nil), r.buf...)
}

// Len is how many values the ring currently holds.
func (r *Ring[T]) Len() int { return len(r.buf) }

// Last is the newest value, and false when the ring is empty.
func (r *Ring[T]) Last() (T, bool) {
	if len(r.buf) == 0 {
		var zero T
		return zero, false
	}
	return r.buf[len(r.buf)-1], true
}
