package serial

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	goserial "go.bug.st/serial"
)

func TestGetBrokerNonNil(t *testing.T) {
	b := GetBroker()
	if b == nil {
		t.Fatal("GetBroker() returned nil")
	}
}

func TestGetBrokerSingleton(t *testing.T) {
	b1 := GetBroker()
	b2 := GetBroker()
	if b1 != b2 {
		t.Fatal("GetBroker() returned different instances")
	}
}

func TestGetBrokerHasScrollback(t *testing.T) {
	b := GetBroker()
	if b.buf == nil {
		t.Fatal("broker.buf is nil; expected initialized scrollback")
	}
	if got := b.buf.Size(); got != scrollbackBytes {
		t.Fatalf("scrollback size = %d, want %d", got, scrollbackBytes)
	}
}

// newTestBroker creates a standalone Broker (not the singleton) for isolated testing.
func newTestBroker() *Broker {
	return &Broker{
		buf: newScrollback(),
	}
}

func TestBrokerWriteInactive(t *testing.T) {
	b := newTestBroker()

	_, err := b.Write([]byte("hello"))
	if err == nil {
		t.Fatal("Write on inactive broker should return error")
	}
	if got := err.Error(); got != "serial port not active" {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestBrokerWriteInactiveAfterClose(t *testing.T) {
	b := newTestBroker()
	b.Close() // no-op on inactive broker

	_, err := b.Write([]byte("hello"))
	if err == nil {
		t.Fatal("Write after Close should return error")
	}
}

func TestBrokerActiveDefault(t *testing.T) {
	b := newTestBroker()
	if b.Active() {
		t.Fatal("new broker should not be active")
	}
}

func TestBrokerSessionCountDefault(t *testing.T) {
	b := newTestBroker()
	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() = %d, want 0", got)
	}
}

// fakePTY simulates a serial port for broker tests that bypass real hardware.
// NOT concurrency-safe; use syncWriter for concurrent tests.
type fakePTY struct {
	bytes.Buffer
}

// syncWriter is a concurrency-safe io.Writer that counts bytes written. Every
// session output in these tests uses one: the pump goroutine writes to it while
// the test reads.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sw *syncWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.Write(p)
}

func (sw *syncWriter) String() string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.String()
}

func (sw *syncWriter) Len() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.Len()
}

// injectSession connects a session to a broker that was activated without a
// real serial device. It calls the real Connect — activateBroker has already
// marked the broker active, so Connect skips startLocked and takes the same
// path production does.
func injectSession(t *testing.T, b *Broker, id string, output io.Writer) *Session {
	t.Helper()

	sess, err := b.Connect(id, output)
	if err != nil {
		t.Fatalf("Connect(%q): %v", id, err)
	}
	return sess
}

// waitFor polls until cond holds, failing the test on timeout. Session output
// now arrives on the session's own goroutine, so assertions on it must wait
// rather than assume the write already landed.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func activateBroker(b *Broker, stdin *fakePTY) {
	b.mu.Lock()
	b.stdin = stdin
	b.active = true
	b.stopCh = make(chan struct{})
	b.mu.Unlock()
}

func TestBrokerWriteActive(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)
	defer b.Close()

	data := []byte("test input")
	n, err := b.Write(data)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}
	if got := stdin.String(); got != "test input" {
		t.Fatalf("stdin got %q, want %q", got, "test input")
	}
}

func TestBrokerDisconnectUnknown(t *testing.T) {
	b := newTestBroker()
	// Disconnecting a non-existent session should not panic.
	b.Disconnect("nonexistent")
	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() = %d, want 0", got)
	}
}

func TestBrokerDisconnectDecrements(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf1, buf2 syncWriter
	injectSession(t, b, "s1", &buf1)
	injectSession(t, b, "s2", &buf2)

	if got := b.SessionCount(); got != 2 {
		t.Fatalf("SessionCount() = %d, want 2", got)
	}

	b.Disconnect("s1")
	if got := b.SessionCount(); got != 1 {
		t.Fatalf("SessionCount() after disconnect = %d, want 1", got)
	}
}

func TestBrokerDisconnectLastStops(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf syncWriter
	injectSession(t, b, "only", &buf)

	b.Disconnect("only")
	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() = %d, want 0", got)
	}
	if b.Active() {
		t.Fatal("broker should be inactive after last session disconnects")
	}
}

func TestBrokerDisconnectStopsDelivery(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf1, buf2 syncWriter
	injectSession(t, b, "s1", &buf1)
	injectSession(t, b, "s2", &buf2)

	b.Disconnect("s1")

	// Publish to the scrollback; only the still-connected session should see it.
	if _, err := b.buf.Write([]byte("after")); err != nil {
		t.Fatalf("scrollback write: %v", err)
	}

	waitFor(t, "s2 to receive", func() bool { return buf2.String() == "after" })
	if buf1.Len() != 0 {
		t.Errorf("s1 received data after disconnect: %q", buf1.String())
	}
}

func TestBrokerCloseDisconnectsAll(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf1, buf2 syncWriter
	s1 := injectSession(t, b, "s1", &buf1)
	s2 := injectSession(t, b, "s2", &buf2)

	b.Close()

	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() after Close = %d, want 0", got)
	}
	if b.Active() {
		t.Fatal("broker should be inactive after Close")
	}

	// Close must join every pump, not just unregister the sessions: a leaked
	// pump would keep writing to a consumer its owner believes is finished.
	for _, sess := range []*Session{s1, s2} {
		select {
		case <-sess.done:
		default:
			t.Fatalf("session %q pump still running after Close", sess.ID)
		}
	}
}

func TestBrokerCloseIdempotent(_ *testing.T) {
	b := newTestBroker()
	// Close on a never-active broker should not panic.
	b.Close()
	b.Close()
}

func TestBrokerWriteAfterClose(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	b.Close()

	_, err := b.Write([]byte("after close"))
	if err == nil {
		t.Fatal("Write after Close should return error")
	}
}

func TestBrokerConnectDuplicateID(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf syncWriter
	injectSession(t, b, "dup", &buf)

	// Connect with same ID should fail on the duplicate check.
	// We can't call b.Connect() because it calls startLocked() in some paths,
	// but since broker is already active, it will skip start and hit the dup check.
	var buf2 syncWriter
	_, err := b.Connect("dup", &buf2)
	if err == nil {
		t.Fatal("Connect with duplicate ID should return error")
	}
	if got := err.Error(); got != `session "dup" already connected` {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestBrokerConnectNewIDWhenActive(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf syncWriter
	sess, err := b.Connect("new-session", &buf)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if sess == nil {
		t.Fatal("Connect returned nil session")
	}
	if sess.ID != "new-session" {
		t.Fatalf("session ID = %q, want %q", sess.ID, "new-session")
	}
	if got := b.SessionCount(); got != 1 {
		t.Fatalf("SessionCount() = %d, want 1", got)
	}

	// Verify the new session receives what the port publishes.
	if _, err := b.buf.Write([]byte("hello")); err != nil {
		t.Fatalf("scrollback write: %v", err)
	}
	waitFor(t, "session output", func() bool { return buf.String() == "hello" })
}

func TestBrokerConnectDisconnectCycle(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf syncWriter
	_, err := b.Connect("cycle", &buf)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}

	if got := b.SessionCount(); got != 1 {
		t.Fatalf("SessionCount() after connect = %d, want 1", got)
	}

	b.Disconnect("cycle")
	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() after disconnect = %d, want 0", got)
	}
}

func TestBrokerConcurrentWrites(t *testing.T) {
	b := newTestBroker()
	stdin := &syncWriter{}
	b.mu.Lock()
	b.stdin = stdin
	b.active = true
	b.stopCh = make(chan struct{})
	b.mu.Unlock()
	defer b.Close()

	const goroutines = 50
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				b.Write([]byte("x"))
			}
		}()
	}
	wg.Wait()

	stdin.mu.Lock()
	got := stdin.buf.Len()
	stdin.mu.Unlock()
	want := goroutines * iterations
	if got != want {
		t.Fatalf("stdin received %d bytes, want %d", got, want)
	}
}

func TestBrokerConcurrentDisconnect(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	const n = 20
	for i := 0; i < n; i++ {
		injectSession(t, b, fmt.Sprintf("s%d", i), &syncWriter{})
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			b.Disconnect(fmt.Sprintf("s%d", i))
		}(i)
	}
	wg.Wait()

	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() = %d, want 0", got)
	}
}

func TestSessionFields(t *testing.T) {
	var buf bytes.Buffer
	s := &Session{ID: "test-id", output: &buf}
	if s.ID != "test-id" {
		t.Fatalf("Session.ID = %q, want %q", s.ID, "test-id")
	}
	if s.output == nil {
		t.Fatal("Session.output is nil")
	}
}

func TestMapLFtoCRLF(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no LF", "hello world", "hello world"},
		{"bare LF", "hello\nworld", "hello\r\nworld"},
		{"already CRLF", "hello\r\nworld", "hello\r\nworld"},
		{"multiple LF", "a\nb\nc", "a\r\nb\r\nc"},
		{"LF at start", "\nhello", "\r\nhello"},
		{"LF at end", "hello\n", "hello\r\n"},
		{"empty", "", ""},
		{"only LF", "\n", "\r\n"},
		{"mixed", "a\r\nb\nc\r\n", "a\r\nb\r\nc\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(mapLFtoCRLF([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("mapLFtoCRLF(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapParity(t *testing.T) {
	tests := []struct {
		input string
		want  goserial.Parity
	}{
		{"none", goserial.NoParity},
		{"even", goserial.EvenParity},
		{"e", goserial.EvenParity},
		{"odd", goserial.OddParity},
		{"o", goserial.OddParity},
		{"mark", goserial.MarkParity},
		{"m", goserial.MarkParity},
		{"space", goserial.SpaceParity},
		{"s", goserial.SpaceParity},
		{"", goserial.NoParity},
		{"bogus", goserial.NoParity},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapParity(tt.input)
			if got != tt.want {
				t.Errorf("mapParity(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapStopBits(t *testing.T) {
	tests := []struct {
		input int
		want  goserial.StopBits
	}{
		{1, goserial.OneStopBit},
		{2, goserial.TwoStopBits},
		{0, goserial.OneStopBit},
		{3, goserial.OneStopBit},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.input), func(t *testing.T) {
			got := mapStopBits(tt.input)
			if got != tt.want {
				t.Errorf("mapStopBits(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// blockingWriter stalls in Write until released, standing in for a consumer
// whose socket has stopped draining (a suspended laptop holding a WebSocket).
type blockingWriter struct {
	release     chan struct{}
	entered     chan struct{}
	once        sync.Once
	releaseOnce sync.Once

	mu   sync.Mutex
	seen bytes.Buffer
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		release: make(chan struct{}),
		entered: make(chan struct{}),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release

	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen.Write(p)
}

// unblock releases a parked writer. Idempotent.
func (w *blockingWriter) unblock() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func (w *blockingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen.String()
}

// wedge registers a session whose consumer is parked inside Write.
func wedge(t *testing.T, b *Broker, id string) *blockingWriter {
	t.Helper()

	w := newBlockingWriter()
	t.Cleanup(w.unblock)
	injectSession(t, b, id, w)

	if _, err := b.buf.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("session %q never reached Write", id)
	}

	return w
}

// The reason for this change: one wedged consumer used to stall the read loop
// for every other consumer, because the fan-out called each writer inline.
func TestStalledSessionDoesNotStallOthers(t *testing.T) {
	b := newTestBroker()
	activateBroker(b, &fakePTY{})

	var healthy syncWriter
	injectSession(t, b, "healthy", &healthy)
	stalled := wedge(t, b, "stalled")

	// The port keeps publishing while that consumer is wedged.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			if _, err := b.buf.Write([]byte("payload")); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing to the scrollback blocked behind a stalled session")
	}

	waitFor(t, "healthy session to keep receiving", func() bool {
		return healthy.Len() >= len("first")+100*len("payload")
	})

	stalled.unblock()
	b.Close()
}

// Connect used to register the session before snapshotting the scrollback,
// with the read loop holding neither lock — so bytes landing in that window
// were delivered live and then again in the replay, out of order. One
// monotonic reader offset makes both impossible.
func TestConnectReplayIsOrderedAndNotDuplicated(t *testing.T) {
	for range 200 {
		b := newTestBroker()
		activateBroker(b, &fakePTY{})

		if _, err := b.buf.Write([]byte("AB")); err != nil {
			t.Fatal(err)
		}

		// Race a publish against the connect, which is exactly the window the
		// old replay path lost bytes in.
		started := make(chan struct{})
		go func() {
			close(started)
			_, _ = b.buf.Write([]byte("CD"))
		}()
		<-started

		var out syncWriter
		if _, err := b.Connect("racer", &out); err != nil {
			t.Fatalf("Connect: %v", err)
		}

		waitFor(t, "replay to complete", func() bool { return out.String() == "ABCD" })

		got := out.String()
		if got != "ABCD" {
			t.Fatalf("session saw %q, want %q (duplicated or reordered seam)", got, "ABCD")
		}

		b.Disconnect("racer")
		b.Close()
	}
}

// A session that falls off the back of the scrollback is resynced to live
// output, and told so rather than handed a silently spliced log.
func TestFallenBehindSessionIsToldAboutTheGap(t *testing.T) {
	b := newTestBroker()
	activateBroker(b, &fakePTY{})

	stalled := wedge(t, b, "stalled")

	// Overrun the whole scrollback while it is parked.
	chunk := make([]byte, 4096)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for range (scrollbackBytes / len(chunk)) + 2 {
		if _, err := b.buf.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}

	stalled.unblock()

	waitFor(t, "drop notice", func() bool {
		return strings.Contains(stalled.String(), "console fell behind")
	})

	// The notice must precede the resynced output, so the seam is visible at
	// the point it happened rather than tacked on somewhere later.
	got := stalled.String()
	notice := strings.Index(got, "[nanokvm: console fell behind")
	if payload := strings.IndexByte(got, 'x'); notice > payload {
		t.Fatalf("drop notice at %d appears after resynced output at %d", notice, payload)
	}

	b.Close()
}

// The multi-streamer's defining property: every session receives the whole
// stream, independently of the others' progress.
func TestEverySessionReceivesTheWholeStream(t *testing.T) {
	b := newTestBroker()
	activateBroker(b, &fakePTY{})
	defer b.Close()

	const sessions = 4

	outs := make([]*syncWriter, sessions)
	for i := range outs {
		outs[i] = &syncWriter{}
		injectSession(t, b, fmt.Sprintf("s%d", i), outs[i])
	}

	var want bytes.Buffer
	for i := range 200 {
		line := fmt.Sprintf("line %03d\r\n", i)
		want.WriteString(line)
		if _, err := b.buf.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	for i, out := range outs {
		waitFor(t, fmt.Sprintf("session %d to drain", i), func() bool {
			return out.Len() >= want.Len()
		})
		if got := out.String(); got != want.String() {
			t.Errorf("session %d received %d bytes, want the full %d-byte stream",
				i, len(got), want.Len())
		}
	}
}
