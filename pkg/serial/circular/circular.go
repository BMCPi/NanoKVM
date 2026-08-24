// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Derived from github.com/siderolabs/go-circular v0.2.3 (circular.go and
// reader.go), reduced to what the serial broker actually needs.
//
// Dropped from upstream: the compressed-chunk index, the on-disk persistence
// layer, the seekable and bounded (non-streaming) reader, and the functional
// options API. That removes every dependency the package had — zap,
// siderolabs/gen and multierr — and removes zstd compression from the write
// path, which upstream performs inline under the buffer lock. On the SG2002's
// single 1 GHz C906 that stall is real, and the history it buys is redundant
// with the always-on capture file.
//
// Changed from upstream: Close wakes readers parked in the condition variable
// (upstream leaves them blocked forever), and Reset discards history by
// raising a floor rather than rewinding the write offset, so offsets stay
// monotonic and no reader can be handed a negative length.

// Package circular provides a fixed-size ring buffer with a single writer and
// any number of independent readers.
//
// The writer never blocks on a reader. A reader that cannot keep up loses the
// bytes that were overwritten while it was behind — and only that reader —
// rather than applying back-pressure to the writer or to its peers.
package circular

import (
	"errors"
	"sync"
)

// ErrClosed is returned by Write on a closed Buffer and by Read on a closed
// Reader.
var ErrClosed = errors.New("circular: closed")

// Buffer is a fixed-size ring of bytes written by one producer and read by any
// number of Readers, each tracking its own position.
//
// Absolute offsets are monotonic for the life of the Buffer: the byte at
// offset x lives at data[x%len(data)] for as long as x is within the most
// recent len(data) bytes written. Readers hold absolute offsets, so falling
// behind is detectable by comparison instead of corrupting a read in progress.
type Buffer struct {
	cond *sync.Cond

	// data is the ring. Its length is fixed at construction.
	data []byte

	// off counts every byte ever written and only ever increases.
	off int64

	// floor is the oldest offset a reader may start from or remain at. Reset
	// raises it to off to discard history without rewinding off.
	floor int64

	// gap holds a new reader back from the write cursor by this many bytes.
	gap int

	closed bool

	// mu guards data, off, floor and closed, and backs cond.
	mu sync.Mutex
}

// NewBuffer returns a Buffer retaining the most recent size bytes.
//
// safetyGap is how far a newly created Reader is held back from the oldest
// retained byte. Without it a reader created at the very back of the ring can
// be overtaken by a write that lands before its first Read, losing bytes it
// was never given a chance to see. It costs that many bytes of history.
func NewBuffer(size, safetyGap int) (*Buffer, error) {
	if size <= 0 {
		return nil, errors.New("circular: size must be positive")
	}
	if safetyGap < 0 || safetyGap >= size {
		return nil, errors.New("circular: safety gap must be in [0, size)")
	}

	buf := &Buffer{
		data: make([]byte, size),
		gap:  safetyGap,
	}
	buf.cond = sync.NewCond(&buf.mu)

	return buf, nil
}

// Write implements io.Writer. It copies p into the ring and wakes every
// waiting Reader.
//
// Write never blocks on a Reader, and never returns a short write: a p longer
// than the ring keeps only its tail, since the rest could not have been read
// before being overwritten regardless.
func (b *Buffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return 0, ErrClosed
	}

	total := len(p)

	if skip := len(p) - len(b.data); skip > 0 {
		// Account for the bytes that would be overwritten by this same call
		// without copying them.
		b.off += int64(skip)
		p = p[skip:]
	}

	i := int(b.off % int64(len(b.data)))
	if tail := len(b.data) - i; tail < len(p) {
		copy(b.data[i:], p[:tail])
		copy(b.data, p[tail:])
	} else {
		copy(b.data[i:i+len(p)], p)
	}

	b.off += int64(len(p))
	b.cond.Broadcast()

	return total, nil
}

// Close marks the Buffer closed. Subsequent writes fail, and readers drain
// what remains and then report io.EOF instead of waiting for more.
//
// Unlike upstream, this wakes readers already parked in Read.
func (b *Buffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed = true
	b.cond.Broadcast()

	return nil
}

// Reset discards the retained history. Readers positioned in the discarded
// range skip forward to the new floor and count the difference as dropped.
//
// The write offset is not rewound, so existing readers cannot be handed a
// position ahead of the writer.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.floor = b.off
}

// Size returns the number of bytes the Buffer retains.
func (b *Buffer) Size() int {
	return len(b.data)
}

// Offset returns the total number of bytes ever written.
func (b *Buffer) Offset() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.off
}

// Len returns the number of bytes currently available to read.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return int(b.off - b.readFloorLocked())
}

// readFloorLocked is the oldest offset still present in the ring.
func (b *Buffer) readFloorLocked() int64 {
	return max(b.floor, b.off-int64(len(b.data)))
}

// startOffLocked is the offset a new Reader begins at: as far back as the ring
// holds, less the safety gap.
func (b *Buffer) startOffLocked() int64 {
	return max(b.floor, b.off-int64(len(b.data)-b.gap), 0)
}
