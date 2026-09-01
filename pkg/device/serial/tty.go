package serial

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// openConsoleTTY opens a console tty as a non-blocking character device in raw
// mode. It is the USB gadget console's open path, deliberately not
// go.bug.st/serial's.
//
// go.bug.st/serial opens O_RDWR|O_NOCTTY|O_NDELAY and then clears non-blocking
// again (serial_unix.go:278), so unixPort.Write is a bare blocking unix.Write
// with no deadline API, and unixPort.Close wakes readers only — never writers.
// On a gser gadget tty that is unusable: u_serial buffers WRITE_BUF_SIZE (8 KB)
// and then blocks the writer indefinitely whenever the host has enumerated the
// device but nothing has opened /dev/ttyUSB0, and gser carries no DTR, so the
// BMC cannot tell those two states apart. A write into that is uncancellable,
// and abandoning it on a goroutine only defers the delivery — the payload still
// lands whenever the port drains, after the caller was told it was dropped.
//
// Keeping the fd non-blocking instead means the Go runtime registers it with
// the netpoller, so (*os.File).SetWriteDeadline works: a write that the port
// will not accept returns os.ErrDeadlineExceeded with the true number of bytes
// that reached it, and not one byte more is ever delivered. Verified on a pty
// slave (a character device, like /dev/ttyGS0) — see TestConsoleTTYWriteDeadline.
//
// go.bug.st/serial stays the open path for real UARTs, where its termios
// handling (baud rate, parity, stop bits, flow control) is load-bearing.
func openConsoleTTY(dev string) (*os.File, error) {
	f, err := os.OpenFile(dev, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := makeRaw(f); err != nil {
		_ = f.Close()
		return nil, err
	}

	// A zero deadline is the cheap pollability probe: poll.FD.SetDeadline
	// returns ErrNoDeadline when the descriptor was never registered with the
	// netpoller, which is the one case where writes here would silently be
	// unbounded again. It cannot happen for a tty, but a device node that
	// turned out not to be one must say so rather than reintroduce the wedge
	// this open path exists to remove.
	if err := f.SetWriteDeadline(time.Time{}); err != nil {
		pkgLog().Warn("serial: console device does not support write deadlines; writes to it are unbounded",
			slog.String("device", dev), slog.Any("err", err))
	}
	return f, nil
}

// makeRaw puts the tty's line discipline in raw mode.
func makeRaw(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("syscall conn: %w", err)
	}
	// Control, not (*os.File).Fd: Fd puts the descriptor back into blocking
	// mode ("On Unix systems this will cause the SetDeadline methods to stop
	// working"), which would silently undo the netpoller registration the
	// write deadline depends on.
	var opErr error
	if err := rc.Control(func(fd uintptr) { opErr = rawTermios(int(fd)) }); err != nil {
		return fmt.Errorf("syscall control: %w", err)
	}
	return opErr
}

// rawTermios applies cfmakeraw(3)'s flag set, minus the framing bits.
//
// u_serial stores the host's SET_LINE_CODING and drops it, so baud rate,
// parity and stop bits genuinely mean nothing on a gadget tty — which is why
// go.bug.st/serial buys nothing here. The line discipline *above* u_serial is a
// different matter: a freshly opened tty inherits the kernel defaults
// (ICANON|ECHO|ISIG in, OPOST|ONLCR out), and a console byte stream that is
// echoed back, buffered until a newline and rewritten on the way out is not a
// console byte stream.
func rawTermios(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("tcgetattr: %w", err)
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	// Deliver whatever has arrived as soon as a byte does, with no inter-byte
	// timer: the read loop publishes into the scrollback, so latency here is
	// latency on every attached console.
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fmt.Errorf("tcsetattr: %w", err)
	}
	return nil
}
