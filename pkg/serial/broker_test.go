package serial

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

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

func TestGetBrokerHasMultiWriter(t *testing.T) {
	b := GetBroker()
	if b.mw == nil {
		t.Fatal("broker.mw is nil; expected initialized MultiWriter")
	}
}

// newTestBroker creates a standalone Broker (not the singleton) for isolated testing.
func newTestBroker() *Broker {
	return &Broker{
		mw: NewMultiWriter(),
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

// syncWriter is a concurrency-safe io.Writer that counts bytes written.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sw *syncWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.Write(p)
}

// injectSession manually wires up a broker to appear active with a fake stdin.
// This avoids calling startLocked() which requires a real serial device.
func injectSession(b *Broker, id string, output *bytes.Buffer) *Session {
	sess := &Session{ID: id, output: output}
	b.sessions.Store(id, sess)
	b.mw.Add(output)
	b.sessionCount.Add(1)
	return sess
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

	var buf1, buf2 bytes.Buffer
	injectSession(b, "s1", &buf1)
	injectSession(b, "s2", &buf2)

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

	var buf bytes.Buffer
	injectSession(b, "only", &buf)

	b.Disconnect("only")
	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() = %d, want 0", got)
	}
	if b.Active() {
		t.Fatal("broker should be inactive after last session disconnects")
	}
}

func TestBrokerDisconnectRemovesFromMultiWriter(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf1, buf2 bytes.Buffer
	injectSession(b, "s1", &buf1)
	injectSession(b, "s2", &buf2)

	b.Disconnect("s1")

	// Write through the multiwriter; only buf2 should receive data.
	b.mw.Write([]byte("after"))
	if buf1.Len() != 0 {
		t.Errorf("buf1 received data after disconnect: %q", buf1.String())
	}
	if got := buf2.String(); got != "after" {
		t.Errorf("buf2: got %q, want %q", got, "after")
	}
}

func TestBrokerCloseDisconnectsAll(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf1, buf2 bytes.Buffer
	injectSession(b, "s1", &buf1)
	injectSession(b, "s2", &buf2)

	b.Close()

	if got := b.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() after Close = %d, want 0", got)
	}
	if b.Active() {
		t.Fatal("broker should be inactive after Close")
	}
	if got := b.mw.Len(); got != 0 {
		t.Fatalf("MultiWriter Len() = %d after Close, want 0", got)
	}
}

func TestBrokerCloseIdempotent(t *testing.T) {
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

	var buf bytes.Buffer
	injectSession(b, "dup", &buf)

	// Connect with same ID should fail on the duplicate check.
	// We can't call b.Connect() because it calls startLocked() in some paths,
	// but since broker is already active, it will skip start and hit the dup check.
	var buf2 bytes.Buffer
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

	var buf bytes.Buffer
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

	// Verify multiwriter has the new session's output.
	b.mw.Write([]byte("hello"))
	if got := buf.String(); got != "hello" {
		t.Fatalf("session output: got %q, want %q", got, "hello")
	}
}

func TestBrokerConnectDisconnectCycle(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	var buf bytes.Buffer
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
		var buf bytes.Buffer
		injectSession(b, fmt.Sprintf("s%d", i), &buf)
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
