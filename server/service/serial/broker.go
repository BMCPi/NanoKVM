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

	"github.com/pi-bmc/nanokvm-app/server/config"
	"github.com/pi-bmc/nanokvm-app/server/telemetry"
)

// Session represents one consumer of the serial port (WebSocket, IPMI SOL, etc.).
type Session struct {
	ID     string
	output io.Writer // receives serial port output
}

// scrollbackSize is the maximum number of bytes retained for new-session replay.
const scrollbackSize = 8192

// Broker manages a single shared serial port connection, allowing multiple
// concurrent sessions to read from and write to it. Modelled after the
// tinkerbell/secondstar shared-terminal pattern.
//
// Architecture:
//
//	                ┌─── WebSocket session ──► ws.WriteMessage
//	serial port ──► │─── IPMI SOL session  ──► UDP sendData
//	  (go serial)   └─── future session    ──► ...
//	      ▲
//	      │  writes (any session)
//	      └── Write()
type Broker struct {
	mu sync.Mutex

	// sessions tracks connected consumers by ID.
	sessions sync.Map // string → *Session

	// multiwriter fans out serial reads to all sessions.
	mw *MultiWriter

	// stdin writes to the serial port (may be the port itself or a wrapper).
	stdin io.Writer

	// serial port handle (native Go, no picocom)
	port   goserial.Port
	active bool
	stopCh chan struct{}

	// sessionCount is an atomic counter for fast len checks and unique ID generation.
	sessionCount atomic.Int32

	// scrollback retains the most recent serial output so new sessions can
	// receive a replay on connect and immediately see the current terminal state.
	scrollMu   sync.Mutex
	scrollback []byte
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
			mw: NewMultiWriter(),
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

	sess := &Session{ID: id, output: output}
	b.sessions.Store(id, sess)
	b.mw.Add(output)
	b.sessionCount.Add(1)

	// Replay scrollback so the new session immediately sees the current
	// terminal state without sending anything to the serial port.
	b.scrollMu.Lock()
	replay := make([]byte, len(b.scrollback))
	copy(replay, b.scrollback)
	b.scrollMu.Unlock()
	if len(replay) > 0 {
		_, _ = output.Write(replay)
	}

	telemetry.SerialSessionOpened(context.Background())
	log.Infof("serial: session %q connected (%d total)", id, b.sessionCount.Load())
	return sess, nil
}

// Disconnect removes a session. If no sessions remain, the serial port
// process is stopped.
func (b *Broker) Disconnect(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	val, loaded := b.sessions.LoadAndDelete(id)
	if !loaded {
		return
	}
	sess := val.(*Session)
	b.mw.Remove(sess.output)
	remaining := b.sessionCount.Add(-1)
	telemetry.SerialSessionClosed(context.Background())

	log.Infof("serial: session %q disconnected (%d remaining)", id, remaining)

	if remaining <= 0 {
		b.stopLocked()
	}
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
	defer b.mu.Unlock()

	b.sessions.Range(func(key, val any) bool {
		sess := val.(*Session)
		b.mw.Remove(sess.output)
		b.sessions.Delete(key)
		return true
	})
	b.sessionCount.Store(0)
	b.stopLocked()

	b.scrollMu.Lock()
	b.scrollback = nil
	b.scrollMu.Unlock()
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

// readLoop reads from the serial port and fans out to all sessions
// via the MultiWriter. Performs LF→CRLF translation on input
// (equivalent to picocom --imap lfcrlf).
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
			b.appendScrollback(mapped)
			_, _ = b.mw.Write(mapped)
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

// appendScrollback appends data to the rolling scrollback buffer, trimming
// the oldest bytes when the buffer exceeds scrollbackSize.
func (b *Broker) appendScrollback(data []byte) {
	b.scrollMu.Lock()
	defer b.scrollMu.Unlock()

	b.scrollback = append(b.scrollback, data...)
	if len(b.scrollback) > scrollbackSize {
		b.scrollback = b.scrollback[len(b.scrollback)-scrollbackSize:]
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
