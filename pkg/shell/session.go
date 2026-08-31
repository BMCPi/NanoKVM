package shell

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	defaultTerm = "xterm-256color"
	defaultCols = 80
	defaultRows = 24
)

// Options configures a session.
type Options struct {
	// Command, when non-empty, is run through the login shell's -c instead of
	// starting an interactive shell. This is the SSH "exec" request (and what
	// `ssh bmc uptime` sends).
	Command string
	// Term is the client's terminal type. Empty defaults to xterm-256color.
	// Ignored when NoPTY is set.
	Term string
	// Cols and Rows are the initial window size. Zero values default to 80x24.
	Cols, Rows uint16
	// NoPTY runs the command on plain pipes instead of a pseudo-terminal —
	// what an SSH client that never sent a pty-req expects.
	NoPTY bool
	// Env holds extra KEY=VALUE entries appended to the base environment.
	Env []string
}

// Session is a running shell (or command) and the file descriptors that talk
// to it. With a PTY, Read/Write are the master side; without one they are the
// child's stdout and stdin, and Stderr is served separately.
type Session struct {
	cmd  *exec.Cmd
	ptmx *os.File

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	waitOnce sync.Once
	waitErr  error
	exitCode int

	closeOnce sync.Once
}

// Start allocates the session's I/O, launches the shell, and returns once the
// process is running. The caller owns the Session and must Close it.
func Start(opts Options) (*Session, error) {
	sh := LoginShell()

	args := []string{"-l"}
	if opts.Command != "" {
		args = []string{"-c", opts.Command}
	}
	// The session's lifetime is the caller's connection, not a context's —
	// Close is what tears the process down (SIGHUP then SIGKILL to the
	// process group). Neither caller has a context that actually maps to
	// that lifetime: the SSH server's request loop (golang.org/x/crypto/ssh)
	// hands this package no context at all, and the WebSocket HTTP handler's
	// request context isn't tied to the hijacked connection -- it only ends
	// when the handler returns, which is after Close has already run. Binding
	// CommandContext to either would be unavailable or a no-op, not a fix.
	//nolint:noctx // no context maps to this session's lifetime; see comment above
	cmd := exec.Command(sh, args...)
	cmd.Dir = Dir()
	cmd.Env = append(baseEnv(sh), opts.Env...)

	s := &Session{cmd: cmd}

	if opts.NoPTY {
		if err := s.startPiped(); err != nil {
			return nil, err
		}
		return s, nil
	}

	term := opts.Term
	if term == "" {
		term = defaultTerm
	}
	cmd.Env = append(cmd.Env, "TERM="+term)

	if err := s.startPTY(opts); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) startPTY(opts Options) error {
	ptmx, pts, err := openPTY()
	if err != nil {
		return err
	}

	s.cmd.Stdin, s.cmd.Stdout, s.cmd.Stderr = pts, pts, pts
	// Setsid + Setctty make the PTY the shell's controlling terminal, which is
	// what gives it job control (^C, ^Z, fg/bg). Ctty indexes the child's fd
	// table, where the pts is fd 0 because it is cmd.Stdin.
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := s.cmd.Start(); err != nil {
		_ = pts.Close()
		_ = ptmx.Close()
		return fmt.Errorf("start %s: %w", s.cmd.Path, err)
	}
	// The child owns the slave end now; keeping it open here would stop the
	// master read from ever seeing EOF after the shell exits.
	_ = pts.Close()

	s.ptmx = ptmx
	s.Resize(opts.Cols, opts.Rows)
	return nil
}

func (s *Session) startPiped() error {
	var err error
	if s.stdin, err = s.cmd.StdinPipe(); err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	if s.stdout, err = s.cmd.StdoutPipe(); err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if s.stderr, err = s.cmd.StderrPipe(); err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	// Its own process group, so Close can signal the command's children too.
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.cmd.Path, err)
	}
	return nil
}

// Pid is the shell's process ID, for logging.
func (s *Session) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// HasPTY reports whether the session runs on a pseudo-terminal.
func (s *Session) HasPTY() bool { return s.ptmx != nil }

func (s *Session) Read(p []byte) (int, error) {
	if s.ptmx != nil {
		return s.ptmx.Read(p)
	}
	return s.stdout.Read(p)
}

func (s *Session) Write(p []byte) (int, error) {
	if s.ptmx != nil {
		return s.ptmx.Write(p)
	}
	return s.stdin.Write(p)
}

// Stderr is the command's standard error, non-nil only for NoPTY sessions —
// a PTY merges it into the master side.
func (s *Session) Stderr() io.Reader { return s.stderr }

// CloseStdin signals EOF to a piped session's standard input.
func (s *Session) CloseStdin() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
}

// Resize pushes the client's window size onto the PTY so full-screen programs
// lay out correctly and SIGWINCH is delivered. No-op without a PTY.
func (s *Session) Resize(cols, rows uint16) {
	if s.ptmx == nil {
		return
	}
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}
	_ = unix.IoctlSetWinsize(int(s.ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

// Signal forwards a signal to the session's process group.
func (s *Session) Signal(sig syscall.Signal) {
	if s.cmd.Process == nil {
		return
	}
	_ = unix.Kill(-s.cmd.Process.Pid, sig)
}

// Wait blocks until the shell exits and reports its exit code. Safe to call
// from several goroutines and after Close; every caller sees the same result.
func (s *Session) Wait() (int, error) {
	s.waitOnce.Do(func() {
		s.waitErr = s.cmd.Wait()
		s.exitCode = s.cmd.ProcessState.ExitCode()
		if s.exitCode < 0 {
			// Killed by a signal; report the shell convention (128 + signum)
			// so SSH clients see something meaningful.
			s.exitCode = 128
			if ws, ok := s.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				s.exitCode = 128 + int(ws.Signal())
			}
		}
	})
	return s.exitCode, s.waitErr
}

// Close tears the session down: SIGHUP then SIGKILL to the whole process group
// (Setsid put the shell in its own session, so children — vi, top — die with
// it), then reaps it. Idempotent.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		if s.cmd.Process != nil {
			pgid := -s.cmd.Process.Pid
			_ = unix.Kill(pgid, unix.SIGHUP)
			_ = unix.Kill(pgid, unix.SIGKILL)
		}
		if s.ptmx != nil {
			_ = s.ptmx.Close()
		}
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
	})
	_, _ = s.Wait()
}
