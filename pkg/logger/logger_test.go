package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

// TestNewFileWriter verifies that configuring a file path yields a rotating
// writer that actually creates and appends to the target file.
func TestNewFileWriter(t *testing.T) {
	// A nested path exercises the directory creation.
	path := filepath.Join(t.TempDir(), "logs", "server.log")

	w, err := newFileWriter(path)
	if err != nil {
		t.Fatalf("newFileWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	if _, ok := any(w).(*lumberjack.Logger); !ok {
		t.Fatalf("expected a *lumberjack.Logger, got %T", w)
	}
	if w.MaxSize <= 0 || w.MaxBackups <= 0 {
		t.Fatalf("rotation not configured: MaxSize=%d MaxBackups=%d", w.MaxSize, w.MaxBackups)
	}

	const line = "hello file logging\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back log file: %v", err)
	}
	if !strings.Contains(string(got), "hello file logging") {
		t.Fatalf("log file missing written line, got %q", got)
	}
}

// TestNewFileWriterUnwritable confirms an unwritable path surfaces an error so
// Init can fall back to stdout rather than silently dropping logs.
func TestNewFileWriterUnwritable(t *testing.T) {
	// A file used as a directory component can't be created into.
	blocker := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := newFileWriter(filepath.Join(blocker, "server.log")); err == nil {
		t.Fatal("expected error for unwritable path, got nil")
	}
}

// TestWriterReflectsConfiguredFile verifies logrus and the shared Writer() are
// pointed at the configured file after the switch selects the file branch. It
// drives the same output-selection logic Init uses without depending on the
// global config singleton.
func TestWriterReflectsConfiguredFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")

	w, err := newFileWriter(path)
	if err != nil {
		t.Fatalf("newFileWriter: %v", err)
	}
	prevOut, prevCloser := output, closer
	t.Cleanup(func() {
		output, closer = prevOut, prevCloser
		logrus.SetOutput(os.Stdout)
		_ = w.Close()
	})

	output = w
	closer = w
	logrus.SetOutput(output)
	logrus.SetFormatter(&formatter{})
	logrus.Info("routed to file")

	if Writer() != w {
		t.Fatal("Writer() did not return the configured file writer")
	}
	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back log file: %v", err)
	}
	if !strings.Contains(string(got), "routed to file") {
		t.Fatalf("logrus output not written to file, got %q", got)
	}
}
