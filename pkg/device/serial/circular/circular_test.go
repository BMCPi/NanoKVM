package circular

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func mustBuffer(t *testing.T, size, gap int) *Buffer {
	t.Helper()
	b, err := NewBuffer(size, gap)
	if err != nil {
		t.Fatalf("NewBuffer(%d, %d): %v", size, gap, err)
	}
	return b
}

func TestNewBufferRejectsBadSizes(t *testing.T) {
	for _, tc := range []struct{ size, gap int }{
		{0, 0},
		{-1, 0},
		{1024, -1},
		{1024, 1024},
		{1024, 2048},
	} {
		if _, err := NewBuffer(tc.size, tc.gap); err == nil {
			t.Errorf("NewBuffer(%d, %d) = nil error, want error", tc.size, tc.gap)
		}
	}
}

func TestWriteAdvancesOffset(t *testing.T) {
	b := mustBuffer(t, 1024, 16)

	for _, size := range []int{0, 1, 512, 1024, 4096} {
		before := b.Offset()
		n, err := b.Write(make([]byte, size))
		if err != nil {
			t.Fatalf("Write(%d): %v", size, err)
		}
		if n != size {
			t.Fatalf("Write(%d) = %d, want %d", size, n, size)
		}
		if got := b.Offset() - before; got != int64(size) {
			t.Fatalf("Write(%d) advanced offset by %d, want %d", size, got, size)
		}
	}
}

// A write longer than the ring keeps its tail — the prefix could not have been
// read before being overwritten anyway.
func TestWriteLargerThanRingKeepsTail(t *testing.T) {
	b := mustBuffer(t, 16, 0)
	r := b.NewReader()

	if _, err := b.Write([]byte("0123456789abcdefGHIJKLMNOPQRSTUV")); err != nil {
		t.Fatal(err)
	}

	got := readN(t, r, 16)
	if want := "GHIJKLMNOPQRSTUV"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReadRoundTrip(t *testing.T) {
	b := mustBuffer(t, 4096, 64)
	r := b.NewReader()
	defer r.Close()

	want := []byte("the quick brown fox jumps over the lazy dog")
	if _, err := b.Write(want); err != nil {
		t.Fatal(err)
	}

	if got := readN(t, r, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Every reader must see the whole stream, independently.
func TestMultipleReadersEachSeeEverything(t *testing.T) {
	b := mustBuffer(t, 64*1024, 256)

	const readers = 8
	want := randomBytes(32 * 1024)

	var wg sync.WaitGroup
	got := make([][]byte, readers)

	for i := range readers {
		r := b.NewReader()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer r.Close()
			got[i] = readN(t, r, len(want))
		}()
	}

	if _, err := b.Write(want); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	for i := range readers {
		if !bytes.Equal(got[i], want) {
			t.Errorf("reader %d received %d bytes, want %d identical", i, len(got[i]), len(want))
		}
	}
}

// Ported from upstream's TestStreamingLateAndIdleReaders ("no chunks" case),
// including its exact expected sizes, so this stays honest to the semantics we
// borrowed: a reader created before the write starts at the oldest retained
// byte (full ring), a reader created after it starts one safety gap in.
func TestLateAndIdleReaderStartPositions(t *testing.T) {
	const (
		size = 65536
		gap  = 256
	)

	b := mustBuffer(t, size, gap)
	idle := b.NewReader()

	data := randomBytes(100000)
	if _, err := b.Write(data); err != nil {
		t.Fatal(err)
	}

	late := b.NewReader()

	for _, tc := range []struct {
		name string
		r    *Reader
		want int
	}{
		{"idle", idle, size},
		{"late", late, size - gap},
	} {
		got := readN(t, tc.r, tc.want)
		if len(got) != tc.want {
			t.Fatalf("%s: read %d bytes, want %d", tc.name, len(got), tc.want)
		}
		if !bytes.Equal(got, data[len(data)-tc.want:]) {
			t.Errorf("%s: content is not the last %d bytes written", tc.name, tc.want)
		}
		if err := tc.r.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The headline property: a reader that never reads must not slow the writer,
// and must not wedge itself either.
func TestStalledReaderDoesNotBlockWriter(t *testing.T) {
	b := mustBuffer(t, 64*1024, 1024)

	stalled := b.NewReader()
	defer stalled.Close()

	chunk := make([]byte, 4096)
	start := time.Now()
	for range 256 { // 1 MiB — 16x the ring
		if _, err := b.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("writer took %s past a stalled reader", elapsed)
	}

	done := make(chan int, 1)
	go func() {
		n, err := stalled.Read(make([]byte, 4096))
		if err != nil {
			done <- -1
			return
		}
		done <- n
	}()

	select {
	case n := <-done:
		if n <= 0 {
			t.Fatal("stalled reader did not resync")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled reader wedged")
	}

	if dropped := stalled.Dropped(); dropped == 0 {
		t.Fatal("Dropped() = 0; a reader that skipped ahead must account for it")
	}
}

func TestDroppedCountsExactly(t *testing.T) {
	b := mustBuffer(t, 1024, 0)
	r := b.NewReader()

	// Overrun the ring by exactly 512 bytes.
	if _, err := b.Write(make([]byte, 1536)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}

	if got := r.Dropped(); got != 512 {
		t.Fatalf("Dropped() = %d, want 512", got)
	}
}

// Upstream leaves readers parked in the condition variable forever when the
// buffer is closed. Ours must not.
func TestBufferCloseWakesParkedReaders(t *testing.T) {
	b := mustBuffer(t, 1024, 0)

	const readers = 4
	errs := make(chan error, readers)

	for range readers {
		r := b.NewReader()
		go func() {
			_, err := r.Read(make([]byte, 64))
			errs <- err
		}()
	}

	time.Sleep(50 * time.Millisecond) // let them park
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	for range readers {
		select {
		case err := <-errs:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("parked reader woke with %v, want io.EOF", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Buffer.Close() left a reader parked")
		}
	}

	if _, err := b.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
}

// A closed buffer still hands over what it already holds before reporting EOF.
func TestBufferCloseDrainsFirst(t *testing.T) {
	b := mustBuffer(t, 1024, 0)
	r := b.NewReader()

	if _, err := b.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "tail" {
		t.Fatalf("got %q, want %q", got, "tail")
	}
}

func TestReaderCloseUnblocksRead(t *testing.T) {
	b := mustBuffer(t, 1024, 0)
	r := b.NewReader()

	errs := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 64))
		errs <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errs:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("got %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reader.Close() did not unblock Read")
	}

	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := r.Read(make([]byte, 64)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read after Close = %v, want ErrClosed", err)
	}
}

// Reset must discard history without rewinding the write offset, or a reader
// parked at the old offset would end up ahead of the writer.
func TestResetDiscardsHistoryWithoutRewinding(t *testing.T) {
	b := mustBuffer(t, 1024, 0)

	if _, err := b.Write([]byte("stale")); err != nil {
		t.Fatal(err)
	}
	before := b.Offset()

	b.Reset()

	if got := b.Offset(); got != before {
		t.Fatalf("Reset rewound the offset to %d, want %d", got, before)
	}
	if got := b.Len(); got != 0 {
		t.Fatalf("Len() = %d after Reset, want 0", got)
	}

	r := b.NewReader()
	defer r.Close()

	if _, err := b.Write([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, r, 5); string(got) != "fresh" {
		t.Fatalf("got %q, want %q; Reset leaked pre-Reset history", got, "fresh")
	}
}

// A reader parked across a Reset must resync rather than read backwards.
func TestResetWithParkedReader(t *testing.T) {
	b := mustBuffer(t, 1024, 0)
	r := b.NewReader()

	read := make(chan []byte, 1)
	go func() {
		p := make([]byte, 64)
		n, err := r.Read(p)
		if err != nil {
			read <- nil
			return
		}
		read <- p[:n]
	}()

	time.Sleep(50 * time.Millisecond)
	b.Reset()
	if _, err := b.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-read:
		if string(got) != "after" {
			t.Fatalf("got %q, want %q", got, "after")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked reader never woke after Reset")
	}
}

// One writer, several readers of varying speed, run under -race.
func TestConcurrentWriterAndReaders(t *testing.T) {
	b := mustBuffer(t, 16*1024, 512)

	const (
		readers = 6
		writes  = 2000
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := range readers {
		r := b.NewReader()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer r.Close()
			p := make([]byte, 1+i*300)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := r.Read(p); err != nil {
					return
				}
			}
		}()
	}

	payload := randomBytes(700)
	for range writes {
		if _, err := b.Write(payload); err != nil {
			t.Fatal(err)
		}
	}

	close(stop)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if want := int64(writes * len(payload)); b.Offset() != want {
		t.Fatalf("Offset() = %d, want %d", b.Offset(), want)
	}
}

// readN reads exactly n bytes or fails the test.
func readN(t *testing.T, r *Reader, n int) []byte {
	t.Helper()

	out := make([]byte, 0, n)
	p := make([]byte, 4096)

	for len(out) < n {
		read, err := r.Read(p[:min(len(p), n-len(out))])
		if err != nil {
			t.Fatalf("Read after %d/%d bytes: %v", len(out), n, err)
		}
		out = append(out, p[:read]...)
	}

	return out
}

func randomBytes(n int) []byte {
	rnd := rand.New(rand.NewSource(1))
	p := make([]byte, n)
	rnd.Read(p)
	return p
}
