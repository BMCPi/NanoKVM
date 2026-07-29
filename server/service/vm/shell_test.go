package vm

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestOpenPTYRunsShell exercises the PTY plumbing behind Shell: allocate a
// pair, run the login shell on it, and confirm a typed command round-trips.
func TestOpenPTYRunsShell(t *testing.T) {
	ptmx, pts, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}

	shell := pickShell()
	cmd := exec.Command(shell, "-l")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.Env = shellEnv(shell)
	cmd.Dir = shellDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := cmd.Start(); err != nil {
		_ = pts.Close()
		_ = ptmx.Close()
		t.Fatalf("start %s: %v", shell, err)
	}
	_ = pts.Close()

	defer func() {
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		_ = ptmx.Close()
		_ = cmd.Wait()
	}()

	setWinsize(ptmx, 100, 40)

	out := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		b := make([]byte, 1024)
		for {
			n, err := ptmx.Read(b)
			if n > 0 {
				buf.Write(b[:n])
				if strings.Contains(buf.String(), "pty-marker-ok") {
					out <- buf.String()
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					out <- buf.String()
				}
				return
			}
		}
	}()

	if _, err := ptmx.Write([]byte("echo pty-marker-ok\n")); err != nil {
		t.Fatalf("write to pty: %v", err)
	}

	select {
	case got := <-out:
		if !strings.Contains(got, "pty-marker-ok") {
			t.Fatalf("shell output missing marker, got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for shell output")
	}
}

func TestPickShellIsExecutable(t *testing.T) {
	sh := pickShell()
	if sh == "" {
		t.Fatal("pickShell returned empty path")
	}
	if _, err := exec.LookPath(sh); err != nil {
		t.Fatalf("pickShell returned %q which is not executable: %v", sh, err)
	}
}
