package serial

// console_test.go covers the two things the USB gadget serial console changed
// about the broker: which device it opens, and what happens to a write nobody
// is draining.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// stubGadgetConsole points the broker's gadget lookup at a fixed answer for
// the duration of one test.
func stubGadgetConsole(t *testing.T, device string) {
	t.Helper()

	orig := gadgetConsoleDevice
	gadgetConsoleDevice = func() string { return device }
	t.Cleanup(func() { gadgetConsoleDevice = orig })
}

// shortWriteTimeout shrinks the write bound so the stalled-port tests finish in
// milliseconds instead of seconds.
func shortWriteTimeout(t *testing.T, d time.Duration) {
	t.Helper()

	orig := writeTimeout
	writeTimeout = d
	t.Cleanup(func() { writeTimeout = orig })
}

// activateWriter is activateBroker for a port that is not a *fakePTY — the
// stalled and character-device ports below.
func activateWriter(b *Broker, w io.Writer) {
	b.mu.Lock()
	b.stdin = w
	b.active = true
	b.stopCh = make(chan struct{})
	b.shutdownCh = make(chan struct{})
	b.mu.Unlock()
}

// With the knob off the gadget reports no console, and the configured device
// is what the port opens — exactly as before this feature existed.
func TestResolveDeviceFallsBackToConfig(t *testing.T) {
	stubGadgetConsole(t, "")

	got, fromGadget := resolveDevice("/dev/ttyS0")
	if got != "/dev/ttyS0" {
		t.Fatalf("resolveDevice = %q, want the configured /dev/ttyS0", got)
	}
	// Load-bearing beyond the name: fromGadget picks the open path, and a
	// real UART must go through go.bug.st/serial for its termios.
	if fromGadget {
		t.Fatal("resolveDevice claimed the configured UART came from the gadget")
	}
}

func TestResolveDeviceUsesGadgetConsole(t *testing.T) {
	stubGadgetConsole(t, "/dev/ttyGS0")

	got, fromGadget := resolveDevice("")
	if got != "/dev/ttyGS0" {
		t.Fatalf("resolveDevice = %q, want /dev/ttyGS0", got)
	}
	// Without this the test would pass on the fallback returning the empty
	// configured device, which is not what its name claims.
	if !fromGadget {
		t.Fatal("resolveDevice did not report /dev/ttyGS0 as the gadget console, so it would be opened as a UART")
	}
}

// The approved policy: enabling the gadget console makes it THE console, even
// when serial.device names a real UART. Nothing is written back to
// Serial.Device, so turning the knob off restores the configured port.
func TestResolveDeviceGadgetWinsOverConfiguredDevice(t *testing.T) {
	stubGadgetConsole(t, "/dev/ttyGS1")

	got, fromGadget := resolveDevice("/dev/ttyS2")
	if got != "/dev/ttyGS1" {
		t.Fatalf("resolveDevice = %q, want the gadget console /dev/ttyGS1 to win", got)
	}
	if !fromGadget {
		t.Fatal("the gadget console won but was not reported as the gadget's")
	}
}

// ConsoleDevice is what the settings UI shows; it must agree with what
// startLocked would open — in both gadget states, and by actually comparing
// with the resolution rather than restating one literal.
func TestConsoleDeviceMatchesResolution(t *testing.T) {
	configured := config.GetInstance().Serial.Device

	for _, gadget := range []string{"/dev/ttyGS0", ""} {
		t.Run("gadget="+strconv.Quote(gadget), func(t *testing.T) {
			stubGadgetConsole(t, gadget)

			wantDevice, wantFromGadget := resolveDevice(configured)
			if got := ConsoleDevice(); got != wantDevice {
				t.Errorf("ConsoleDevice() = %q, want %q — the UI and the broker would disagree about the port in use", got, wantDevice)
			}
			gotDevice, gotFromGadget := ConsoleDeviceInfo()
			if gotDevice != wantDevice || gotFromGadget != wantFromGadget {
				t.Errorf("ConsoleDeviceInfo() = (%q, %v), want (%q, %v)",
					gotDevice, gotFromGadget, wantDevice, wantFromGadget)
			}
		})
	}
}

// stalledPort stands in for a u_serial port whose 8 KB write buffer is
// full and whose host is not draining it: the write never returns until the
// test releases it. It is a plain io.Writer, so it has no write deadline —
// the go.bug.st/serial case, not the gadget one.
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

// waitForCall blocks until the port has been entered want times.
func (w *stalledPort) waitForCall(t *testing.T, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.callCount() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("port was entered %d times, want %d", w.callCount(), want)
}

// The gser gadget has no DTR, so the BMC cannot tell "host enumerated" from
// "host has the port open": a keystroke from the WebSocket pump or the SOL
// engine must not be able to pin its goroutine forever behind another one.
func TestBrokerWriteGivesUpOnBlockedPort(t *testing.T) {
	shortWriteTimeout(t, 50*time.Millisecond)

	stdin := newStalledPort()
	defer close(stdin.release)

	b := newTestBroker()
	activateWriter(b, stdin)

	// The first write is inside the port and cannot be recalled; that is the
	// point of not abandoning it.
	go func() { _, _ = b.Write([]byte("a")) }()
	stdin.waitForCall(t, 1)

	// A follow-up keystroke arriving behind it is refused, bounded by the
	// timeout, rather than queueing up behind a port that may never drain.
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

// REGRESSION (critical): the write slot must not outlive the port.
//
// Repro that wedged the console permanently: one write the port never took,
// then serial.Restart() — which ANY serial settings save triggers — and every
// subsequent write on the freshly opened, healthy port failed with "waiting
// behind an earlier write" for the life of the process, killing the web
// terminal and IPMI SOL input while blaming a good port.
func TestWriteSlotDoesNotOutlivePort(t *testing.T) {
	shortWriteTimeout(t, 50*time.Millisecond)

	stalled := newStalledPort()
	b := newTestBroker()
	activateWriter(b, stalled)

	// A write the port never accepts. It holds the slot for as long as it is
	// in there.
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		_, _ = b.Write([]byte("reboot\r"))
	}()
	stalled.waitForCall(t, 1)

	if _, err := b.Write([]byte("x")); !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("write behind the parked one = %v, want ErrWriteTimeout", err)
	}

	// The port dies and a healthy one replaces it — Restart's Close() then the
	// next Connect()'s startLocked().
	b.mu.Lock()
	b.stopLocked()
	b.mu.Unlock()

	healthy := &fakePTY{}
	activateBroker(b, healthy)

	n, err := b.Write([]byte("ok"))
	if err != nil {
		t.Fatalf("write on a fresh, healthy port failed: %v — the dead port's write slot outlived it", err)
	}
	if n != 2 {
		t.Fatalf("Write returned %d, want 2", n)
	}
	if got := healthy.String(); got != "ok" {
		t.Fatalf("healthy port got %q, want %q", got, "ok")
	}

	// And releasing the old port's write drains into its own retired slot
	// rather than deadlocking or disturbing the new one.
	close(stalled.release)
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("the write parked on the retired port never returned")
	}
	if _, err := b.Write([]byte("!")); err != nil {
		t.Fatalf("write after the retired port drained: %v", err)
	}
}

// The ordinary case must stay ordinary: a port that accepts the write returns
// its byte count, and the bytes arrive — including the next one, so the slot
// is provably released rather than merely un-erroring.
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

	if n, err := b.Write([]byte("!")); err != nil || n != 1 {
		t.Fatalf("second Write = (%d, %v), want (1, nil)", n, err)
	}
	if got := stdin.String(); got != "hello!" {
		t.Fatalf("port got %q after the second write, want %q", got, "hello!")
	}
}

// ── The character-device write deadline ─────────────────────────────────
//
// This is the whole basis of the bounded write: that (*os.File).SetWriteDeadline
// really fires on a character device. A pty slave is one, and its master is a
// host that has enumerated the port and is not draining it — exactly the gser
// steady state. If these two tests ever fail, the design has to go back to
// abandoning writes and admitting that a "dropped" payload may still land.

// openPTYPair allocates a pty the way openpty(3) does, with both ends
// non-blocking so both carry deadlines.
func openPTYPair(t *testing.T) (master *os.File, slaveName string) {
	t.Helper()

	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("no pty available in this environment: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })

	rc, err := ptmx.SyscallConn()
	if err != nil {
		t.Fatalf("pty master SyscallConn: %v", err)
	}
	var (
		num   int
		ioErr error
	)
	if err := rc.Control(func(fd uintptr) {
		if ioErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioErr != nil {
			return
		}
		num, ioErr = unix.IoctlGetInt(int(fd), unix.TIOCGPTN)
	}); err != nil {
		t.Fatalf("pty master Control: %v", err)
	}
	if ioErr != nil {
		t.Fatalf("pty ioctl: %v", ioErr)
	}
	return ptmx, fmt.Sprintf("/dev/pts/%d", num)
}

// drain reads everything available from f within window.
func drain(t *testing.T, f *os.File, window time.Duration) []byte {
	t.Helper()

	if err := f.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64*1024)
	var out []byte
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out
		}
	}
}

// The primitive, asserted directly: a deadline on a character device returns
// os.ErrDeadlineExceeded with the TRUE partial count, and delivers nothing
// afterwards.
func TestConsoleTTYWriteDeadline(t *testing.T) {
	shortWriteTimeout(t, 200*time.Millisecond)

	master, slaveName := openPTYPair(t)
	port, err := openConsoleTTY(slaveName)
	if err != nil {
		t.Fatalf("openConsoleTTY(%s): %v", slaveName, err)
	}
	defer func() { _ = port.Close() }()

	if fi, statErr := port.Stat(); statErr != nil || fi.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("the port under test is not a character device (%v, %v)", fi, statErr)
	}
	// If this ever errors, the descriptor was never registered with the
	// netpoller and every write below is silently unbounded.
	if err := port.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("character device does not support write deadlines: %v", err)
	}

	// 1 MiB is far more than any tty buffer, and nothing reads the master.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = 'a'
	}
	n, err := writeBounded(port, payload)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write to an undrained character device = (%d, %v), want os.ErrDeadlineExceeded", n, err)
	}
	if n <= 0 || n >= len(payload) {
		t.Fatalf("write returned n=%d, want a true partial count in (0, %d)", n, len(payload))
	}

	// Exactly n bytes reached the port, and nothing arrives after the
	// deadline: no phantom delivery of what the caller was told was dropped.
	if got := drain(t, master, 500*time.Millisecond); len(got) != n {
		t.Fatalf("host drained %d bytes but the write reported %d delivered", len(got), n)
	}
	if extra := drain(t, master, 300*time.Millisecond); len(extra) != 0 {
		t.Fatalf("%d bytes arrived after the write had already given up", len(extra))
	}

	// The port stays usable: the deadline is per-write, not terminal.
	if n, err := writeBounded(port, []byte("still here")); err != nil || n != 10 {
		t.Fatalf("write after a deadline = (%d, %v), want (10, nil)", n, err)
	}
}

// REGRESSION: a write the broker reports as dropped must not land later.
//
// Before this fix Write handed the payload to a goroutine and abandoned it, so
// `reboot\r` reached the managed host after the operator had been told it was
// dropped, telemetry never counted it, and an IPMI SOL retransmit arrived as a
// second in-order copy.
func TestBrokerWriteDropsNothingIntoTheFuture(t *testing.T) {
	shortWriteTimeout(t, 200*time.Millisecond)

	master, slaveName := openPTYPair(t)
	port, err := openConsoleTTY(slaveName)
	if err != nil {
		t.Fatalf("openConsoleTTY(%s): %v", slaveName, err)
	}
	defer func() { _ = port.Close() }()

	b := newTestBroker()
	activateWriter(b, port)

	// One console line the host must never see, at the tail of more input than
	// the port can possibly take while nobody drains it. The master is
	// deliberately never read before the write: this is the gser steady state,
	// a host that has enumerated the device and is not reading it.
	const command = "reboot\r"
	payload := append(bytes.Repeat([]byte("a"), 1<<20), command...)

	n, err := b.Write(payload)
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("write to a stalled port = (%d, %v), want ErrWriteTimeout", n, err)
	}
	if n >= len(payload) {
		t.Fatalf("the undrained port took all %d bytes; the stall was never reproduced", n)
	}

	// Whatever the host eventually drains must be exactly what Write admitted
	// to sending, and must not contain the command at the tail — a command
	// executing on the managed host after the operator was told it had been
	// refused is the safety problem this design exists to remove.
	delivered := drain(t, master, 500*time.Millisecond)
	extra := drain(t, master, 300*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("%d bytes landed after Write had already given up on them", len(extra))
	}
	received := make([]byte, 0, len(delivered)+len(extra))
	received = append(append(received, delivered...), extra...)
	if bytes.Contains(received, []byte(command)) {
		t.Fatalf("the host received %q from a write the caller was told was dropped", command)
	}
	if len(delivered) != n {
		t.Fatalf("host received %d bytes, but Write reported %d delivered", len(delivered), n)
	}
}
