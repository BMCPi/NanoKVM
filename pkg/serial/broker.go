// Package serial provides a shared serial terminal broker that allows
// multiple concurrent consumers (WebSocket, IPMI SOL, Redfish) to read
// from and write to the same serial port.
package serial

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	goserial "go.bug.st/serial"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/serial/circular"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
)

// Session represents one consumer of the serial port (WebSocket, IPMI SOL, etc.).
//
// Each session owns a reader into the shared scrollback and a goroutine
// pumping it to output. That decoupling is the point: a consumer that stops
// draining (a suspended laptop holding a WebSocket open) falls behind in its
// own reader instead of stalling the port for everybody else.
type Session struct {
	ID     string
	output io.Writer // receives serial port output
	reader *circular.Reader
	done   chan struct{} // closed when the pump goroutine exits
}

const (
	// scrollbackBytes is the history a newly attached session replays. The
	// SG2002 has 256 MB with 158 MB reserved for the multimedia subsystem, so
	// this is ~0.25% of what is left — worth it for ~2500 lines of context on
	// connect. Retaining the whole boot log is the always-on capture's job.
	scrollbackBytes = 256 * 1024

	// scrollbackSafetyGap holds a new reader one pump chunk clear of the write
	// cursor, so a burst landing between Connect and the pump's first read
	// cannot overtake it.
	scrollbackSafetyGap = 4096

	// pumpChunkBytes is the per-session read size, matched to readLoop's.
	pumpChunkBytes = 4096

	// sessionDrainTimeout bounds how long Disconnect and Close wait for a
	// session's pump to exit. Its reader is already closed by then, so it
	// returns as soon as the consumer's in-flight write does — comfortably
	// inside this for the WebSocket writer's own 10s deadline. The bound
	// exists for a consumer whose Write never returns at all, which must not
	// be able to hang a settings save (Restart runs from an HTTP handler).
	sessionDrainTimeout = 15 * time.Second
)

// Broker manages a single shared serial port connection, allowing multiple
// concurrent sessions to read from and write to it. Modelled after the
// tinkerbell/secondstar shared-terminal pattern.
//
// Output is published once into a shared scrollback and pulled from it by each
// session at its own pace, rather than pushed to each consumer inline. The read
// loop therefore never waits on a consumer: a session that stops draining falls
// behind in its own reader, where the cost is borne by that session alone. The
// alternative — writing to every consumer from the read loop — lets one stalled
// socket back up into the port's kernel buffer and drop host output for
// everybody, which is exactly what happens when the host is spewing and someone
// left a console tab open on a sleeping laptop.
//
// Architecture:
//
//	                                        ┌── reader ──► WebSocket session ──► ws.WriteMessage
//	serial port ──► readLoop ──► scrollback │── reader ──► IPMI SOL session  ──► UDP sendData
//	  (go serial)                (circular) └── reader ──► capture session   ──► capture file
//	      ▲
//	      │  writes (any session)
//	      └── Write()
type Broker struct {
	mu sync.Mutex

	// sessions tracks connected consumers by ID.
	sessions sync.Map // string → *Session

	// buf holds the shared scrollback. readLoop writes to it; every session
	// reads from it at its own pace.
	buf *circular.Buffer

	// stdin writes to the serial port (may be the port itself or a wrapper).
	stdin io.Writer

	// serial port handle (native Go, no picocom)
	port   goserial.Port
	active bool
	stopCh chan struct{}

	// shutdownCh unblocks reopen's paced retry loop so Close (and therefore
	// StopCapture, which calls it via Disconnect's remaining<=0 path) can
	// interrupt it immediately instead of leaving it to run
	// captureRetryInterval to completion. startLocked creates a fresh one on
	// every activation, since a broker is reused across a Restart (Close then
	// a later Connect/startLocked) and must be interruptible again each time;
	// unlike stopCh it is never touched by stopLocked -- reopen calls
	// stopLocked on the dead port at the very top of its own retry loop, and
	// that must not trip the very signal it is about to select on.
	shutdownCh chan struct{}

	// sessionCount is an atomic counter for fast len checks and unique ID generation.
	sessionCount atomic.Int32
}

// newScrollback allocates the shared history buffer.
func newScrollback() *circular.Buffer {
	buf, err := circular.NewBuffer(scrollbackBytes, scrollbackSafetyGap)
	if err != nil {
		// Both arguments are constants above, so this is unreachable unless
		// they are edited into an invalid pair — which any test that builds a
		// broker turns into an immediate, obvious failure.
		panic(err)
	}
	return buf
}

// pkgLogHolder is pkg/serial's holder for the "serial" component logger.
// StartCapture (capture.go) Sets it; every Broker/Session method below reads
// through pkgLog() instead of a stored field, so whichever singleton is
// touched first, the real, component-tagged logger StartCapture was given
// always wins — see logger.Holder's doc comment for why a sync.Once-guarded
// var would get this wrong.
var pkgLogHolder logger.Holder

// pkgLog returns the package's component logger, defaulting to the process
// logger if StartCapture has not run yet.
func pkgLog() *slog.Logger {
	return pkgLogHolder.Get()
}

// singleton broker instance
var (
	broker     *Broker
	brokerOnce sync.Once
)

// GetBroker returns the singleton Broker instance, constructing it on first call.
func GetBroker() *Broker {
	brokerOnce.Do(func() {
		broker = &Broker{
			buf: newScrollback(),
		}
	})
	return broker
}

// Connect registers a new session with the given ID and output writer.
// If this is the first session, the serial port process is started.
// Returns the session for later disconnection.
func (b *Broker) Connect(id string, output io.Writer) (*Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Check for duplicate session ID.
	if _, loaded := b.sessions.Load(id); loaded {
		return nil, fmt.Errorf("session %q already connected", id)
	}

	// Start the serial port process if not already running.
	if !b.active {
		if err := b.startLocked(); err != nil {
			return nil, fmt.Errorf("start serial: %w", err)
		}
	}

	sess := &Session{
		ID:     id,
		output: output,
		reader: b.buf.NewReader(),
		done:   make(chan struct{}),
	}
	b.sessions.Store(id, sess)
	b.sessionCount.Add(1)

	// The reader starts as far back as the scrollback holds and runs on into
	// live output through one monotonic offset, so the new session sees the
	// current terminal state and then the live stream, in order. There is no
	// separate replay write that could race the read loop and duplicate or
	// reorder the seam.
	go sess.pump()

	// context.Background() is deliberate here and at the three other
	// telemetry.* calls below (Disconnect, Write, readLoop), not an omission.
	// These record broker-lifetime counters, not anything scoped to a single
	// caller's request: a session opened by one WebSocket connection is later
	// closed by an unrelated Disconnect call (or never, if the process exits
	// first), and Write/readLoop run on goroutines with no request context of
	// their own to begin with. Binding any of them to a caller's context
	// would make the record vanish exactly when that caller went away, while
	// the broker -- and the traffic it was still moving -- kept running.
	telemetry.SerialSessionOpened(context.Background())
	pkgLog().Info("serial: session connected", slog.String("session", id), slog.Int("total", int(b.sessionCount.Load())))
	return sess, nil
}

// Disconnect removes a session. If no sessions remain, the serial port
// process is stopped.
func (b *Broker) Disconnect(id string) {
	b.mu.Lock()
	val, loaded := b.sessions.LoadAndDelete(id)
	if !loaded {
		b.mu.Unlock()
		return
	}
	sess, ok := val.(*Session)
	if !ok {
		b.mu.Unlock()
		return
	}
	remaining := b.sessionCount.Add(-1)
	sess.stop()
	if remaining <= 0 {
		b.stopLocked()
	}
	b.mu.Unlock()

	// Join the pump outside b.mu. It may be parked in a consumer write with a
	// deadline of its own, and a wedged consumer must not hold the broker lock
	// against every other session's Connect and Disconnect.
	sess.wait()

	telemetry.SerialSessionClosed(context.Background())
	pkgLog().Info("serial: session disconnected", slog.String("session", id), slog.Int("remaining", int(remaining)))
}

// Write sends data to the serial port. Safe to call from any goroutine.
func (b *Broker) Write(data []byte) (int, error) {
	b.mu.Lock()
	stdin := b.stdin
	b.mu.Unlock()

	if stdin == nil {
		return 0, fmt.Errorf("serial port not active")
	}
	n, err := stdin.Write(data)
	telemetry.SerialBytesTx(context.Background(), n)
	return n, err
}

// Active reports whether the serial port process is running.
func (b *Broker) Active() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.active
}

// SessionCount returns the number of connected sessions.
func (b *Broker) SessionCount() int {
	return int(b.sessionCount.Load())
}

// Close forcibly shuts down the broker, disconnecting all sessions.
func (b *Broker) Close() {
	b.mu.Lock()
	b.closeShutdownLocked()
	var stopped []*Session
	b.sessions.Range(func(key, val any) bool {
		if sess, ok := val.(*Session); ok {
			sess.stop()
			stopped = append(stopped, sess)
		}
		b.sessions.Delete(key)
		return true
	})
	b.sessionCount.Store(0)
	b.stopLocked()
	b.mu.Unlock()

	// Join the pumps outside b.mu, as Disconnect does.
	for _, sess := range stopped {
		sess.wait()
	}

	// Drop the history. Close is how Restart changes the port framing, and
	// bytes captured at the old baud rate would replay as garbage at the new
	// one. Offsets are not rewound, so any straggling reader stays consistent.
	b.buf.Reset()
}

// mapParity converts config parity string to go.bug.st/serial parity mode.
func mapParity(parity string) goserial.Parity {
	switch parity {
	case "even", "e":
		return goserial.EvenParity
	case "odd", "o":
		return goserial.OddParity
	case "mark", "m":
		return goserial.MarkParity
	case "space", "s":
		return goserial.SpaceParity
	default:
		return goserial.NoParity
	}
}

// mapStopBits converts config stop bits int to go.bug.st/serial stop bits.
func mapStopBits(bits int) goserial.StopBits {
	switch bits {
	case 2:
		return goserial.TwoStopBits
	default:
		return goserial.OneStopBit
	}
}

// startLocked opens the serial port with the configured parameters.
// Caller must hold b.mu.
func (b *Broker) startLocked() error {
	cfg := config.GetInstance()
	device := cfg.Serial.Device

	mode := &goserial.Mode{
		BaudRate: cfg.Serial.BaudRate,
		DataBits: cfg.Serial.DataBits,
		Parity:   mapParity(cfg.Serial.Parity),
		StopBits: mapStopBits(cfg.Serial.StopBits),
	}

	port, err := goserial.Open(device, mode)
	if err != nil {
		return fmt.Errorf("open serial %s: %w", device, err)
	}

	b.port = port
	b.stdin = port
	b.active = true
	b.stopCh = make(chan struct{})
	b.shutdownCh = make(chan struct{})

	go b.readLoop()

	pkgLog().Info("serial: opened port (native)", slog.String("device", device), slog.Int("baud", cfg.Serial.BaudRate))
	return nil
}

// closeShutdownLocked closes shutdownCh if it exists and is not already
// closed. Caller must hold b.mu. A no-op nil check covers Close on a broker
// that was never activated (startLocked never ran); the closed-check covers a
// second Close call on an already-closed broker -- both existing,
// intentional idempotency cases this must not break.
func (b *Broker) closeShutdownLocked() {
	if b.shutdownCh == nil {
		return
	}
	select {
	case <-b.shutdownCh:
	default:
		close(b.shutdownCh)
	}
}

// stopLocked closes the serial port.
// Caller must hold b.mu.
func (b *Broker) stopLocked() {
	if !b.active {
		return
	}

	b.active = false
	close(b.stopCh)

	if b.port != nil {
		_ = b.port.Close()
	}
	b.port = nil
	b.stdin = nil

	pkgLog().Info("serial: closed")
}

// readLoop reads from the serial port and publishes to the shared scrollback,
// from which each session's pump draws at its own pace. Performs LF→CRLF
// translation on input (equivalent to picocom --imap lfcrlf).
func (b *Broker) readLoop() {
	buf := make([]byte, 4096)

	for {
		select {
		case <-b.stopCh:
			return
		default:
		}

		n, err := b.port.Read(buf)
		if err != nil {
			select {
			case <-b.stopCh:
			default:
				// Unexpected death (EIO, device glitch). Reopen while
				// consumers remain — with the always-on capture session
				// registered, a one-off read error must not silently end
				// capture for the rest of the server's lifetime.
				pkgLog().Warn("serial: read error; reopening", slog.Any("err", err))
				go b.reopen()
			}
			return
		}

		if n > 0 {
			telemetry.SerialBytesRx(context.Background(), n)
			// Map LF → CRLF for terminal display (like picocom --imap lfcrlf).
			mapped := mapLFtoCRLF(buf[:n])
			// A memcpy and a broadcast — never a consumer's write. This is
			// what keeps a stalled session from backing up into the port's
			// kernel buffer and dropping host output for everyone.
			_, _ = b.buf.Write(mapped)
		}
	}
}

// reopen tears down a port whose read loop died and reopens it while any
// consumer remains registered (with the always-on capture session that is the
// norm). Paced retries cover a device that needs a moment to come back --
// interruptibly, via shutdownCh, so Close/StopCapture can cut this short
// instead of it running captureRetryInterval to completion after everybody
// has already left.
func (b *Broker) reopen() {
	b.mu.Lock()
	b.stopLocked()
	shutdown := b.shutdownCh
	b.mu.Unlock()

	for {
		if b.SessionCount() == 0 {
			return // last consumer left; nothing to reopen for
		}
		select {
		case <-shutdown:
			return // broker closed while this retry loop was running
		default:
		}
		b.mu.Lock()
		if b.active {
			b.mu.Unlock()
			return // somebody else (a new Connect) already reopened it
		}
		err := b.startLocked()
		b.mu.Unlock()
		if err == nil {
			pkgLog().Info("serial: reopened after read error")
			return
		}
		select {
		case <-shutdown:
			return
		case <-time.After(captureRetryInterval):
		}
	}
}

// pump copies this session's view of the scrollback — retained history first,
// then live output — to its consumer, until the reader is closed or the
// consumer's writer fails.
func (s *Session) pump() {
	defer close(s.done)

	buf := make([]byte, pumpChunkBytes)
	var reported int64

	for {
		n, err := s.reader.Read(buf)

		if n > 0 {
			// A session that falls far enough behind is resynced to live
			// output. Mark the seam: silently splicing a console log is how an
			// operator ends up debugging from a record that never happened.
			if dropped := s.reader.Dropped(); dropped != reported {
				notice := fmt.Sprintf("\r\n[nanokvm: console fell behind, %d bytes dropped]\r\n", dropped-reported)
				reported = dropped
				if _, werr := s.output.Write([]byte(notice)); werr != nil {
					return
				}
			}
			if _, werr := s.output.Write(buf[:n]); werr != nil {
				// The consumer is gone. Stop pumping rather than retrying a
				// dead writer for the life of the session; its owner (the
				// WebSocket handler, the SOL reaper) calls Disconnect.
				return
			}
		}

		if err != nil {
			return
		}
	}
}

// stop closes the session's reader, unblocking its pump. Safe on a Session
// built without one (tests construct bare Sessions).
func (s *Session) stop() {
	if s.reader != nil {
		_ = s.reader.Close()
	}
}

// wait blocks until the session's pump goroutine has exited, giving up after
// sessionDrainTimeout. An abandoned pump can still write to its consumer after
// this returns; that consumer is already being torn down by its owner, and the
// alternative is blocking the broker on it forever.
func (s *Session) wait() {
	if s.done == nil {
		return
	}

	select {
	case <-s.done:
	case <-time.After(sessionDrainTimeout):
		pkgLog().Warn("serial: session did not drain; abandoning its pump", slog.String("session", s.ID), slog.Duration("timeout", sessionDrainTimeout))
	}
}

// mapLFtoCRLF replaces bare LF (not preceded by CR) with CRLF.
// This is equivalent to picocom's --imap lfcrlf.
func mapLFtoCRLF(data []byte) []byte {
	// Fast path: if no LF present, return as-is.
	if !bytes.ContainsRune(data, '\n') {
		return data
	}

	var out bytes.Buffer
	out.Grow(len(data) + 16)
	for i, b := range data {
		//nolint:gosec // i ranges over [0,len(data)) and the i==0 check short-circuits before data[i-1] is evaluated, so i-1 is always in [0,len(data)-2] here
		if b == '\n' && (i == 0 || data[i-1] != '\r') {
			out.WriteByte('\r')
		}
		out.WriteByte(b)
	}
	return out.Bytes()
}
