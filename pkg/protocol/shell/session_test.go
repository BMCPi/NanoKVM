package shell

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// readUntil pumps the session until marker shows up or the deadline passes.
func readUntil(t *testing.T, s *Session, marker string) string {
	t.Helper()

	out := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		b := make([]byte, 1024)
		for {
			n, err := s.Read(b)
			if n > 0 {
				buf.Write(b[:n])
				if strings.Contains(buf.String(), marker) {
					out <- buf.String()
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					out <- buf.String()
				}
				return
			}
		}
	}()

	select {
	case got := <-out:
		return got
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %q", marker)
		return ""
	}
}

// TestSessionPTYRoundTrip exercises the PTY path: a typed command reaches the
// shell and its output comes back on the master side.
func TestSessionPTYRoundTrip(t *testing.T) {
	s, err := Start(Options{Cols: 100, Rows: 40})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	if !s.HasPTY() {
		t.Fatal("expected a PTY session")
	}

	if _, err := s.Write([]byte("echo pty-marker-ok\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readUntil(t, s, "pty-marker-ok"); !strings.Contains(got, "pty-marker-ok") {
		t.Fatalf("output missing marker, got %q", got)
	}
}

// TestSessionPTYWindowSize confirms the winsize ioctl reaches the child: the
// shell's own COLUMNS/LINES must match what Start was given.
func TestSessionPTYWindowSize(t *testing.T) {
	s, err := Start(Options{Cols: 123, Rows: 45})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	if _, err := s.Write([]byte("echo size=${COLUMNS}x${LINES}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readUntil(t, s, "size=123x45")
	if !strings.Contains(got, "size=123x45") {
		t.Fatalf("window size not applied, got %q", got)
	}
}

// TestSessionExecNoPTY covers the SSH "exec without pty-req" path, including
// the exit code and stderr being reported separately.
func TestSessionExecNoPTY(t *testing.T) {
	s, err := Start(Options{Command: "echo out-marker; echo err-marker 1>&2; exit 7", NoPTY: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	if s.HasPTY() {
		t.Fatal("expected a pipe session")
	}

	stdout, _ := io.ReadAll(s)
	stderr, _ := io.ReadAll(s.Stderr())

	code, _ := s.Wait()
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if !strings.Contains(string(stdout), "out-marker") {
		t.Errorf("stdout = %q, want out-marker", stdout)
	}
	if !strings.Contains(string(stderr), "err-marker") {
		t.Errorf("stderr = %q, want err-marker", stderr)
	}
}

// TestSessionCloseIsIdempotent guards the teardown path used by every caller's
// defer: repeated Closes must not block or panic, and the exit code observed
// after the command finished on its own must survive them.
func TestSessionCloseIsIdempotent(t *testing.T) {
	s, err := Start(Options{Command: "true", NoPTY: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if code, _ := s.Wait(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	s.Close()
	s.Close()

	if code, _ := s.Wait(); code != 0 {
		t.Errorf("exit code after Close = %d, want 0", code)
	}
}

func TestLoginShellIsExecutable(t *testing.T) {
	sh := LoginShell()
	if sh == "" {
		t.Fatal("LoginShell returned empty path")
	}
	if _, err := exec.LookPath(sh); err != nil {
		t.Fatalf("LoginShell returned %q which is not executable: %v", sh, err)
	}
}
