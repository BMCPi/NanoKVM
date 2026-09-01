package streamio

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// counting_test.go pins the reader that drives every byte-level progress bar
// in the UI. Its whole job is reporting partial work, so the case that matters
// is the one where a Read returns bytes *and* an error.

func TestCountingReaderReportsEachRead(t *testing.T) {
	var seen []int64
	r := NewCountingReader(strings.NewReader("hello world"), func(n int64) {
		seen = append(seen, n)
	})

	buf := make([]byte, 5)
	total := 0
	for {
		n, err := r.Read(buf)
		total += n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if total != 11 {
		t.Errorf("read %d bytes, want 11", total)
	}

	var reported int64
	for _, n := range seen {
		reported += n
	}
	if reported != 11 {
		t.Errorf("reported %d bytes, want 11", reported)
	}
}

// io.Reader may return n > 0 alongside an error, including io.EOF. Bytes
// delivered on that final read are real bytes; dropping them leaves a progress
// bar permanently short of its total and a transfer that looks stalled at 99%.
func TestCountingReaderCountsBytesDeliveredWithAnError(t *testing.T) {
	var reported int64
	r := NewCountingReader(&lastGaspReader{data: []byte("tail")}, func(n int64) {
		reported += n
	})

	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 4 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %d, %v; want 4, EOF", n, err)
	}
	if reported != 4 {
		t.Errorf("reported %d bytes, want 4 — bytes delivered alongside an "+
			"error are still bytes", reported)
	}
}

// A read that delivered nothing is not progress, and reporting a zero would
// wake every observer for no reason.
func TestCountingReaderStaysQuietOnAnEmptyRead(t *testing.T) {
	calls := 0
	r := NewCountingReader(strings.NewReader(""), func(int64) { calls++ })
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	if calls != 0 {
		t.Errorf("onRead called %d times for a read that delivered nothing", calls)
	}
}

// A nil callback is the ordinary case for a caller that wants no progress, and
// must not be a panic.
func TestCountingReaderToleratesANilCallback(t *testing.T) {
	r := NewCountingReader(strings.NewReader("data"), nil)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
}

// lastGaspReader returns all its data and io.EOF in the same call.
type lastGaspReader struct{ data []byte }

func (r *lastGaspReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, io.EOF
}
