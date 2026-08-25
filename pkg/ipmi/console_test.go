package ipmi

import (
	"bytes"
	"context"
	"testing"
)

func TestConsoleSingleActivation(t *testing.T) {
	broker := &fakeBroker{}
	c := &consoleHAL{broker: broker}

	conn, err := c.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := c.Open(context.Background()); err == nil {
		t.Fatal("second activation succeeded; a shared serial port cannot serve two")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Deactivation re-arms.
	conn, err = c.Open(context.Background())
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	conn.Close()
}

func TestConsoleDataPlane(t *testing.T) {
	broker := &fakeBroker{}
	c := &consoleHAL{broker: broker}

	conn, err := c.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	// Keystrokes go to the serial port.
	if _, err := conn.Write([]byte("reboot\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := broker.keystroke.String(); got != "reboot\r" {
		t.Errorf("keystrokes = %q, want %q", got, "reboot\r")
	}

	// Serial output accumulates and drains without blocking.
	buf := make([]byte, 4)
	if n, _ := conn.ReadAvailable(buf); n != 0 {
		t.Errorf("ReadAvailable on empty buffer = %d, want 0", n)
	}
	broker.out.Write([]byte("login:"))
	var got bytes.Buffer
	for {
		n, err := conn.ReadAvailable(buf)
		if err != nil {
			t.Fatalf("read available: %v", err)
		}
		if n == 0 {
			break
		}
		got.Write(buf[:n])
	}
	if got.String() != "login:" {
		t.Errorf("console output = %q, want %q", got.String(), "login:")
	}
}

func TestConsoleBufferBounded(t *testing.T) {
	broker := &fakeBroker{}
	c := &consoleHAL{broker: broker}

	conn, err := c.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	chunk := bytes.Repeat([]byte{'x'}, 4096)
	for range 32 {
		broker.out.Write(chunk)
	}
	broker.out.Write([]byte("END"))

	cc := conn.(*consoleConn)
	cc.mu.Lock()
	size := len(cc.buf)
	tail := string(cc.buf[len(cc.buf)-3:])
	cc.mu.Unlock()
	if size > solBufferSize {
		t.Errorf("buffer grew to %d, cap is %d", size, solBufferSize)
	}
	// Overflow drops the oldest bytes, never the newest.
	if tail != "END" {
		t.Errorf("buffer tail = %q, want END", tail)
	}
}
