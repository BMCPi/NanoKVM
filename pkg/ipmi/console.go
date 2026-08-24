package ipmi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/bougou/go-ipmi/pkg/hal"
)

// solBrokerID names the SOL session on the shared serial broker, so it shows
// up alongside web-console sessions and the capture session.
const solBrokerID = "ipmi-sol"

// solBufferSize bounds console output buffered between SOL packets. The SOL
// data plane drains it on every exchange; a host spewing faster than the
// remote console acks loses the oldest bytes, which for a console is the
// right failure mode (a live prompt beats a complete backlog).
const solBufferSize = 64 * 1024

// consoleHAL exposes the host serial console to the framework's SOL payload
// engine through the shared serial broker, so an SOL session coexists with
// web-console viewers instead of fighting them for the port.
type consoleHAL struct {
	broker   consoleBroker
	attached atomic.Bool
}

func (c *consoleHAL) Open(ctx context.Context) (hal.ConsoleConn, error) {
	// One activation at a time (spec §15.3); the framework closes the conn
	// on deactivation or session teardown, which re-arms this.
	if !c.attached.CompareAndSwap(false, true) {
		return nil, errors.New("sol console already attached")
	}
	conn := &consoleConn{hal: c}
	if _, err := c.broker.Connect(solBrokerID, (*consoleSink)(conn)); err != nil {
		c.attached.Store(false)
		return nil, err
	}
	return conn, nil
}

// consoleConn is one SOL activation: Write carries remote-console keystrokes
// to the serial port, ReadAvailable drains buffered serial output.
type consoleConn struct {
	hal *consoleHAL

	mu     sync.Mutex
	buf    []byte
	closed bool
}

// consoleSink is the io.Writer registered with the broker for serial output.
// It is a separate view of consoleConn because both directions are "Write":
// the broker writes serial output in, the SOL engine writes keystrokes out.
type consoleSink consoleConn

func (s *consoleSink) Write(p []byte) (int, error) {
	c := (*consoleConn)(s)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	if over := len(c.buf) - solBufferSize; over > 0 {
		c.buf = c.buf[over:]
	}
	return len(p), nil
}

func (c *consoleConn) Write(p []byte) (int, error) {
	return c.hal.broker.Write(p)
}

func (c *consoleConn) ReadAvailable(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := copy(p, c.buf)
	if n > 0 {
		c.buf = c.buf[:copy(c.buf, c.buf[n:])]
	}
	return n, nil
}

func (c *consoleConn) SendBreak(ctx context.Context) error {
	return hal.ErrNotSupported
}

func (c *consoleConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.buf = nil
	c.mu.Unlock()

	c.hal.broker.Disconnect(solBrokerID)
	c.hal.attached.Store(false)
	return nil
}
