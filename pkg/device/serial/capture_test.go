package serial

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureWriterAppendsAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log", "console.log")
	w := newCaptureWriter(path, 32, slog.New(slog.DiscardHandler))

	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("capture file not created: %v", err)
	}
	if string(data) != "0123456789" {
		t.Fatalf("capture = %q, want the written bytes", data)
	}

	// Cross the 32-byte cap: the current file must rotate to .1 and a new
	// generation must start on the next write.
	if _, err := w.Write([]byte("abcdefghijklmnopqrstuvwxyz")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if _, err := w.Write([]byte("next")); err != nil {
		t.Fatalf("write after rotate: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "next" {
		t.Fatalf("new generation = %q, want %q", data, "next")
	}
	rotated, _ := os.ReadFile(path + ".1")
	if string(rotated) != "0123456789abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("rotated = %q, want the full first generation", rotated)
	}
	w.Close()
}

func TestCaptureWriterSurvivesUnwritablePath(t *testing.T) {
	// A capture failure must never disturb the broker fan-out: Write reports
	// full success even when the file cannot be opened.
	w := newCaptureWriter("/proc/nonexistent/console.log", 1024, slog.New(slog.DiscardHandler))
	n, err := w.Write([]byte("data"))
	if err != nil || n != 4 {
		t.Fatalf("Write = (%d, %v), want (4, nil)", n, err)
	}
	w.Close()
}
