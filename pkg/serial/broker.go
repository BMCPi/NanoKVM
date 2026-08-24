// Package serial provides a shared serial terminal broker that allows
// multiple concurrent consumers (WebSocket, IPMI SOL, Redfish) to read
// from and write to the same serial port.
package serial

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	goserial "go.bug.st/serial"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
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

// singleton broker instance
var (
	broker     *Broker
	brokerOnce sync.Once
)

// GetBroker returns the singleton Broker instance.
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

	telemetry.SerialSessionOpened(context.Background())
	log.Infof("serial: session %q connected (%d total)", id, b.sessionCount.Load())
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
	log.Infof("serial: session %q disconnected (%d remaining)", id, remaining)
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

	go b.readLoop()

	log.Infof("serial: opened %s @ %d baud (native)", device, cfg.Serial.BaudRate)
	return nil
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

	log.Info("serial: closed")
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
				log.Warnf("serial: read error: %s; reopening", err)
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
// norm). Paced retries cover a device that needs a moment to come back.
func (b *Broker) reopen() {
	b.mu.Lock()
	b.stopLocked()
	b.mu.Unlock()

	for {
		if b.SessionCount() == 0 {
			return // last consumer left; nothing to reopen for
		}
		b.mu.Lock()
		if b.active {
			b.mu.Unlock()
			return // somebody else (a new Connect) already reopened it
		}
		err := b.startLocked()
		b.mu.Unlock()
		if err == nil {
			log.Info("serial: reopened after read error")
			return
		}
		time.Sleep(captureRetryInterval)
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
		log.Warnf("serial: session %q did not drain within %s; abandoning its pump", s.ID, sessionDrainTimeout)
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
		if b == '\n' && (i == 0 || data[i-1] != '\r') {
			out.WriteByte('\r')
		}
		out.WriteByte(b)
	}
	return out.Bytes()
}
