package streamio

// counting.go is the reader behind every byte-level progress bar in the UI.
//
// It lives here rather than beside either caller because both of them are
// counting the same thing — an HTTP body on its way to the data partition —
// and the interesting rule is easy to get wrong in exactly one way, twice: a
// Read may deliver bytes *and* an error in the same call, and dropping those
// bytes leaves a bar permanently short of its total on a transfer that in fact
// completed.

import "io"

// CountingReader reports every byte read to a callback, for progress display.
//
// The callback runs on the reading goroutine, inside the copy loop, so it must
// be cheap and must not block: a callback that takes a contended lock throttles
// the transfer it is measuring.
type CountingReader struct {
	r      io.Reader
	onRead func(n int64)
}

// NewCountingReader wraps r. A nil onRead is allowed and makes this a plain
// pass-through, so a caller with no progress to report needs no special case.
func NewCountingReader(r io.Reader, onRead func(n int64)) *CountingReader {
	return &CountingReader{r: r, onRead: onRead}
}

func (cr *CountingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	// Before the error check, deliberately: io.Reader is allowed to return
	// n > 0 with a non-nil error, io.EOF included, and those bytes are real.
	if n > 0 && cr.onRead != nil {
		cr.onRead(int64(n))
	}
	return n, err
}
