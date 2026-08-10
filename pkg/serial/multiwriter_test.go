package serial

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMultiWriterEmpty(t *testing.T) {
	mw := NewMultiWriter()
	data := []byte("hello")
	n, err := mw.Write(data)
	if err != nil {
		t.Fatalf("Write to empty MultiWriter returned error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}
}

func TestMultiWriterBroadcast(t *testing.T) {
	var buf1, buf2, buf3 bytes.Buffer
	mw := NewMultiWriter(&buf1, &buf2, &buf3)

	data := []byte("broadcast")
	n, err := mw.Write(data)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}

	for i, buf := range []*bytes.Buffer{&buf1, &buf2, &buf3} {
		if got := buf.String(); got != "broadcast" {
			t.Errorf("writer %d: got %q, want %q", i, got, "broadcast")
		}
	}
}

func TestMultiWriterMultipleWrites(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	mw := NewMultiWriter(&buf1, &buf2)

	for _, msg := range []string{"one", "two", "three"} {
		if _, err := mw.Write([]byte(msg)); err != nil {
			t.Fatalf("Write(%q) error: %v", msg, err)
		}
	}

	want := "onetwothree"
	for i, buf := range []*bytes.Buffer{&buf1, &buf2} {
		if got := buf.String(); got != want {
			t.Errorf("writer %d: got %q, want %q", i, got, want)
		}
	}
}

func TestMultiWriterAddRemove(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	mw := NewMultiWriter(&buf1)

	mw.Write([]byte("a"))
	mw.Add(&buf2)
	mw.Write([]byte("b"))
	mw.Remove(&buf1)
	mw.Write([]byte("c"))

	if got := buf1.String(); got != "ab" {
		t.Errorf("buf1: got %q, want %q", got, "ab")
	}
	if got := buf2.String(); got != "bc" {
		t.Errorf("buf2: got %q, want %q", got, "bc")
	}
}

// errWriter always returns an error on Write.
type errWriter struct{ err error }

func (e *errWriter) Write([]byte) (int, error) { return 0, e.err }

func TestMultiWriterBestEffort(t *testing.T) {
	var good1, good2 bytes.Buffer
	bad := &errWriter{err: errors.New("broken pipe")}

	mw := NewMultiWriter(&good1, bad, &good2)

	data := []byte("survive")
	n, err := mw.Write(data)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}

	if got := good1.String(); got != "survive" {
		t.Errorf("good1: got %q, want %q", got, "survive")
	}
	if got := good2.String(); got != "survive" {
		t.Errorf("good2: got %q, want %q", got, "survive")
	}
}

func TestMultiWriterBestEffortAllBroken(t *testing.T) {
	bad1 := &errWriter{err: errors.New("err1")}
	bad2 := &errWriter{err: errors.New("err2")}
	mw := NewMultiWriter(bad1, bad2)

	data := []byte("ignored")
	n, err := mw.Write(data)
	if err != nil {
		t.Fatalf("Write returned error even with all broken writers: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}
}

func TestMultiWriterLen(t *testing.T) {
	var b1, b2, b3 bytes.Buffer
	mw := NewMultiWriter()

	if got := mw.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}

	mw.Add(&b1)
	mw.Add(&b2)
	if got := mw.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	mw.Add(&b3)
	if got := mw.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}

	mw.Remove(&b2)
	if got := mw.Len(); got != 2 {
		t.Fatalf("Len() = %d after Remove, want 2", got)
	}
}

func TestMultiWriterLenWithInitial(t *testing.T) {
	var b1, b2 bytes.Buffer
	mw := NewMultiWriter(&b1, &b2)
	if got := mw.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
}

func TestMultiWriterRemoveNonExistent(t *testing.T) {
	var b1, b2 bytes.Buffer
	mw := NewMultiWriter(&b1)

	// Removing a writer that was never added should be a no-op.
	mw.Remove(&b2)

	if got := mw.Len(); got != 1 {
		t.Fatalf("Len() = %d after removing non-existent writer, want 1", got)
	}

	// Verify original writer still works.
	mw.Write([]byte("ok"))
	if got := b1.String(); got != "ok" {
		t.Errorf("buf1: got %q, want %q", got, "ok")
	}
}

func TestMultiWriterDoubleAdd(t *testing.T) {
	var b1 bytes.Buffer
	mw := NewMultiWriter()

	mw.Add(&b1)
	mw.Add(&b1) // same writer added twice is idempotent (map key)
	if got := mw.Len(); got != 1 {
		t.Fatalf("Len() = %d after double add, want 1 (map dedup)", got)
	}
}

func TestMultiWriterDoubleRemove(t *testing.T) {
	var b1 bytes.Buffer
	mw := NewMultiWriter(&b1)

	mw.Remove(&b1)
	mw.Remove(&b1) // second remove is a no-op
	if got := mw.Len(); got != 0 {
		t.Fatalf("Len() = %d after double remove, want 0", got)
	}
}

func TestMultiWriterEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	mw := NewMultiWriter(&buf)

	n, err := mw.Write([]byte{})
	if err != nil {
		t.Fatalf("Write(empty) error: %v", err)
	}
	if n != 0 {
		t.Fatalf("Write(empty) returned %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("buf has %d bytes, want 0", buf.Len())
	}
}

func TestMultiWriterLargePayload(t *testing.T) {
	var buf bytes.Buffer
	mw := NewMultiWriter(&buf)

	data := make([]byte, 1<<16) // 64 KiB
	for i := range data {
		data[i] = byte(i % 256)
	}

	n, err := mw.Write(data)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("large payload mismatch")
	}
}

// countWriter counts total bytes written (thread-safe).
type countWriter struct{ n atomic.Int64 }

func (cw *countWriter) Write(p []byte) (int, error) {
	cw.n.Add(int64(len(p)))
	return len(p), nil
}

func TestMultiWriterConcurrent(t *testing.T) {
	mw := NewMultiWriter()
	const writers = 20
	const iterations = 500

	var wg sync.WaitGroup

	// Goroutines that add/remove writers concurrently.
	cws := make([]*countWriter, writers)
	for i := range cws {
		cws[i] = &countWriter{}
	}

	// Add writers concurrently.
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			mw.Add(cws[i])
		}(i)
	}
	wg.Wait()

	// Concurrent writes.
	wg.Add(iterations)
	for i := 0; i < iterations; i++ {
		go func() {
			defer wg.Done()
			mw.Write([]byte("x"))
		}()
	}
	wg.Wait()

	// Remove half the writers concurrently.
	wg.Add(writers / 2)
	for i := 0; i < writers/2; i++ {
		go func(i int) {
			defer wg.Done()
			mw.Remove(cws[i])
		}(i)
	}
	wg.Wait()

	if got := mw.Len(); got != writers/2 {
		t.Errorf("Len() = %d, want %d", got, writers/2)
	}
}

func TestMultiWriterConcurrentAddRemoveWrite(t *testing.T) {
	mw := NewMultiWriter()
	const goroutines = 50
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Concurrent adders.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				w := &countWriter{}
				mw.Add(w)
			}
		}()
	}

	// Concurrent writers.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				mw.Write([]byte("data"))
			}
		}()
	}

	// Concurrent removers (removing countWriter instances that may or may not exist).
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				mw.Remove(&countWriter{})
			}
		}()
	}

	wg.Wait()
	// If we reach here without panic or data race, the test passes.
}

func TestMultiWriterImplementsWriterInterface(t *testing.T) {
	mw := NewMultiWriter()
	var _ io.Writer = mw // compile-time check
}
