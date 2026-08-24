// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Derived from github.com/siderolabs/go-circular v0.2.3 (reader.go). See the
// provenance note in circular.go.

package circular

import (
	"io"
	"sync/atomic"
)

// Reader is an independent read position within a Buffer. It reads the
// retained history first and then continues into live output through the same
// offset, so history and live data arrive in order and can never overlap.
//
// A Reader is not safe for concurrent use: give each consumer its own.
type Reader struct {
	buf *Buffer

	// off is the reader's absolute position, guarded by buf.mu.
	off int64

	// dropped counts bytes overwritten before this reader reached them,
	// guarded by buf.mu.
	dropped int64

	closed atomic.Bool
}

// NewReader returns a Reader starting as far back in the retained history as
// the safety gap allows.
func (b *Buffer) NewReader() *Reader {
	b.mu.Lock()
	defer b.mu.Unlock()

	return &Reader{buf: b, off: b.startOffLocked()}
}

// Read implements io.Reader. It blocks until data is available, the Reader is
// closed (ErrClosed) or the Buffer is closed and drained (io.EOF).
//
// A Reader that has fallen behind the writer resyncs to the oldest retained
// byte and counts the skipped bytes in Dropped, rather than returning data
// that has already been overwritten.
func (r *Reader) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	b := r.buf

	b.mu.Lock()
	defer b.mu.Unlock()

	for {
		if r.closed.Load() {
			return 0, ErrClosed
		}

		if floor := b.readFloorLocked(); r.off < floor {
			r.dropped += floor - r.off
			r.off = floor
		}

		if r.off < b.off {
			break
		}
		if b.closed {
			return 0, io.EOF
		}

		b.cond.Wait()
	}

	// r.off is at or after the read floor, so the gap to the writer is never
	// more than one ring length and the copy below cannot wrap twice.
	n := int(min(b.off-r.off, int64(len(p))))

	i := int(r.off % int64(len(b.data)))
	if tail := len(b.data) - i; tail < n {
		copy(p, b.data[i:])
		copy(p[tail:], b.data[:n-tail])
	} else {
		copy(p, b.data[i:i+n])
	}

	r.off += int64(n)

	return n, nil
}

// Close unblocks a Read waiting on this Reader and makes further reads return
// ErrClosed. It is idempotent and never returns an error.
func (r *Reader) Close() error {
	if r.closed.Swap(true) {
		return nil
	}

	r.buf.mu.Lock()
	defer r.buf.mu.Unlock()

	r.buf.cond.Broadcast()

	return nil
}

// Dropped returns the cumulative number of bytes overwritten before this
// Reader reached them. A consumer can use it to tell its user the stream has a
// seam rather than presenting a spliced log as continuous.
func (r *Reader) Dropped() int64 {
	r.buf.mu.Lock()
	defer r.buf.mu.Unlock()

	return r.dropped
}

// Offset returns the Reader's absolute position in the Buffer.
func (r *Reader) Offset() int64 {
	r.buf.mu.Lock()
	defer r.buf.mu.Unlock()

	return r.off
}
