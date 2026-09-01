package serial

// console_test.go covers the two things the USB gadget serial console changed
// about the broker: which device it opens, and what happens to a write nobody
// is draining.

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubGadgetConsole points the broker's gadget lookup at a fixed answer for
// the duration of one test.
func stubGadgetConsole(t *testing.T, device string) {
	t.Helper()

	orig := gadgetConsoleDevice
	gadgetConsoleDevice = func() string { return device }
	t.Cleanup(func() { gadgetConsoleDevice = orig })
}

// With the knob off the gadget reports no console, and the configured device
// is what the port opens — exactly as before this feature existed.
func TestResolveDeviceFallsBackToConfig(t *testing.T) {
	stubGadgetConsole(t, "")

	if got := resolveDevice("/dev/ttyS0"); got != "/dev/ttyS0" {
		t.Fatalf("resolveDevice = %q, want the configured /dev/ttyS0", got)
	}
}

func TestResolveDeviceUsesGadgetConsole(t *testing.T) {
	stubGadgetConsole(t, "/dev/ttyGS0")

	if got := resolveDevice(""); got != "/dev/ttyGS0" {
		t.Fatalf("resolveDevice = %q, want /dev/ttyGS0", got)
	}
}

// The approved policy: enabling the gadget console makes it THE console, even
// when serial.device names a real UART. Nothing is written back to
// Serial.Device, so turning the knob off restores the configured port.
func TestResolveDeviceGadgetWinsOverConfiguredDevice(t *testing.T) {
	stubGadgetConsole(t, "/dev/ttyGS1")

	if got := resolveDevice("/dev/ttyS2"); got != "/dev/ttyGS1" {
		t.Fatalf("resolveDevice = %q, want the gadget console /dev/ttyGS1 to win", got)
	}
}

// ConsoleDevice is what the settings UI shows; it must agree with what
// startLocked would open.
func TestConsoleDeviceMatchesResolution(t *testing.T) {
	stubGadgetConsole(t, "/dev/ttyGS0")

	if got := ConsoleDevice(); got != "/dev/ttyGS0" {
		t.Fatalf("ConsoleDevice() = %q, want /dev/ttyGS0", got)
	}
}

// stalledPort stands in for a u_serial port whose 8 KB write buffer is
// full and whose host is not draining it: the write never returns until the
// test releases it.
type stalledPort struct {
	release chan struct{}

	mu    sync.Mutex
	calls int
}

func newStalledPort() *stalledPort {
	return &stalledPort{release: make(chan struct{})}
}

func (w *stalledPort) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()

	<-w.release
	return len(p), nil
}

func (w *stalledPort) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// The gser gadget has no DTR, so the BMC cannot tell "host enumerated" from
// "host has the port open": a keystroke from the WebSocket pump or the SOL
// engine must not be able to pin its goroutine forever.
func TestBrokerWriteGivesUpOnBlockedPort(t *testing.T) {
	orig := writeTimeout
	writeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { writeTimeout = orig })

	stdin := newStalledPort()
	defer close(stdin.release)

	b := newTestBroker()
	b.mu.Lock()
	b.stdin = stdin
	b.active = true
	b.stopCh = make(chan struct{})
	b.shutdownCh = make(chan struct{})
	b.mu.Unlock()

	returned := make(chan error, 1)
	go func() {
		_, err := b.Write([]byte("a"))
		returned <- err
	}()

	select {
	case err := <-returned:
		if !errors.Is(err, ErrWriteTimeout) {
			t.Fatalf("Write error = %v, want ErrWriteTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a port nobody is draining")
	}

	// A follow-up keystroke while the first write is still parked is dropped
	// too — bounded by the same timeout — rather than queueing up another
	// abandoned goroutine behind it.
	second := make(chan error, 1)
	go func() {
		_, err := b.Write([]byte("b"))
		second <- err
	}()

	var err error
	select {
	case err = <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("second Write blocked behind the parked one")
	}
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("second Write error = %v, want ErrWriteTimeout", err)
	}
	if got := stdin.callCount(); got != 1 {
		t.Errorf("port saw %d writes, want 1 — the second keystroke should never have reached it", got)
	}
	if !strings.Contains(err.Error(), "serial") {
		t.Errorf("error %q does not say which subsystem dropped the write", err)
	}
}

// The ordinary case must stay ordinary: a port that accepts the write returns
// its byte count, unchanged.
func TestBrokerWriteUnblockedIsUnaffected(t *testing.T) {
	b := newTestBroker()
	stdin := &fakePTY{}
	activateBroker(b, stdin)

	n, err := b.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Fatalf("Write returned %d, want 5", n)
	}
	if got := stdin.String(); got != "hello" {
		t.Fatalf("port got %q, want %q", got, "hello")
	}

	// And the write slot is released, so the next keystroke is not refused.
	if _, err := b.Write([]byte("!")); err != nil {
		t.Fatalf("second Write: %v", err)
	}
}
