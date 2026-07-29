package vm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// shellCandidates are tried in order when picking the login shell for the
// BMC terminal. The image ships bash; busybox's ash is the fallback.
var shellCandidates = []string{"/bin/bash", "/bin/sh"}

// Shell upgrades the HTTP connection to a WebSocket and bridges it to an
// interactive shell running on the BMC itself, on its own pseudo-terminal.
//
// This is the counterpart to Terminal: that one attaches to the target
// host's serial port, this one is a local shell. Each connection gets its
// own PTY and its own shell process; closing the socket kills the session.
//
// Wire protocol matches Terminal so the same xterm.js client code works:
// text frames are keystrokes, binary frames are JSON WinSize resizes, and
// everything the shell prints comes back as binary frames.
func (s *Service) Shell(c *gin.Context) {
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Errorf("failed to init websocket: %s", err)
		return
	}
	defer func() {
		_ = ws.Close()
	}()

	ptmx, pts, err := openPTY()
	if err != nil {
		log.Errorf("failed to allocate pty: %s", err)
		_ = ws.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
		return
	}

	shell := pickShell()
	// The session's lifetime is the WebSocket's, not a context's — the
	// deferred kill below is what tears the shell down.
	cmd := exec.Command(shell, "-l")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.Env = shellEnv(shell)
	cmd.Dir = shellDir()
	// Setsid + Setctty make the PTY the shell's controlling terminal, which
	// is what gives it job control (^C, ^Z, fg/bg). Ctty indexes the child's
	// fd table, where the pts is fd 0 because it is cmd.Stdin.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := cmd.Start(); err != nil {
		log.Errorf("failed to start shell %s: %s", shell, err)
		_ = ws.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
		_ = pts.Close()
		_ = ptmx.Close()
		return
	}
	// The child owns the slave end now; keeping it open here would stop the
	// master read from ever seeing EOF after the shell exits.
	_ = pts.Close()

	log.Infof("bmc shell session started: %s (pid %d)", shell, cmd.Process.Pid)

	defer func() {
		// Signal the whole process group — Setsid put the shell in its own
		// session, so children (vi, top, …) die with it.
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGHUP)
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		_ = ptmx.Close()
		_ = cmd.Wait()
		log.Debugf("bmc shell session ended (pid %d)", cmd.Process.Pid)
	}()

	// Start at a sane size; the client sends the real one on connect.
	setWinsize(ptmx, 80, 24)

	// PTY → WebSocket. Closing the socket here unblocks the read loop below
	// when the shell exits on its own (e.g. the user typed `exit`).
	go func() {
		_, _ = io.Copy(&wsWriter{ws: ws}, ptmx)
		_ = ws.Close()
	}()

	// WebSocket → PTY. No read deadline: a shell session can idle for hours.
	var zeroTime time.Time
	_ = ws.SetReadDeadline(zeroTime)

	for {
		msgType, p, err := ws.ReadMessage()
		if err != nil {
			return
		}

		if msgType == websocket.BinaryMessage {
			var winSize WinSize
			if json.Unmarshal(p, &winSize) == nil && winSize.Cols > 0 && winSize.Rows > 0 {
				setWinsize(ptmx, winSize.Cols, winSize.Rows)
			}
			continue
		}

		if _, err := ptmx.Write(p); err != nil {
			return
		}
	}
}

// openPTY allocates a pseudo-terminal pair the way openpty(3) does, without
// pulling in cgo: open the multiplexer, unlock the slave, resolve its index.
func openPTY() (*os.File, *os.File, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	fd := int(ptmx.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		_ = ptmx.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}

	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		_ = ptmx.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", err)
	}

	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = ptmx.Close()
		return nil, nil, fmt.Errorf("open pts: %w", err)
	}

	return ptmx, pts, nil
}

// setWinsize pushes the client's terminal dimensions onto the PTY so
// full-screen programs lay out correctly and SIGWINCH is delivered.
func setWinsize(ptmx *os.File, cols, rows uint16) {
	ws := &unix.Winsize{Row: rows, Col: cols}
	if err := unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		log.Debugf("pty resize to %dx%d failed: %s", cols, rows, err)
	}
}

func pickShell() string {
	for _, sh := range shellCandidates {
		if fi, err := os.Stat(sh); err == nil && !fi.IsDir() {
			return sh
		}
	}
	return "/bin/sh"
}

func shellEnv(shell string) []string {
	home := shellDir()
	return []string{
		"TERM=xterm-256color",
		"HOME=" + home,
		"SHELL=" + shell,
		"USER=root",
		"LOGNAME=root",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

// shellDir is the shell's working directory — /root when it exists (the
// BMC runs as root), otherwise the filesystem root.
func shellDir() string {
	if fi, err := os.Stat("/root"); err == nil && fi.IsDir() {
		return "/root"
	}
	return "/"
}
