package logger

import (
	"bytes"
	"log/slog"
	"testing"
)

// TestHolderUpgradesRatherThanLatches is the regression test for the bug a
// sync.Once-guarded package var has: an early reader must not permanently
// pin the holder to the process default, locking out the real logger Set
// (i.e., Start) supplies afterwards.
func TestHolderUpgradesRatherThanLatches(t *testing.T) {
	var h Holder

	// Get before any Set returns the process default, not a zero value and
	// not a panic.
	if got := h.Get(); got != slog.Default() {
		t.Fatalf("Get before Set = %p, want the process default %p", got, slog.Default())
	}

	var buf bytes.Buffer
	injected := slog.New(slog.NewTextHandler(&buf, nil))
	h.Set(injected)

	// The read that happened before Set must not have latched: Get now
	// returns what Set just stored, not the earlier default.
	if got := h.Get(); got != injected {
		t.Fatalf("Get after Set = %p, want the injected logger %p", got, injected)
	}

	// A second Set (e.g. a Restart re-deriving the logger) still wins.
	second := slog.New(slog.NewTextHandler(&buf, nil))
	h.Set(second)
	if got := h.Get(); got != second {
		t.Fatalf("Get after second Set = %p, want %p", got, second)
	}
}

// TestHolderSetNilFallsBackToDefault mirrors Or's nil-guard: a holder fed a
// nil logger (a hand-built test fixture, say) still returns a usable one.
func TestHolderSetNilFallsBackToDefault(t *testing.T) {
	var h Holder
	h.Set(nil)
	if got := h.Get(); got != slog.Default() {
		t.Fatalf("Get after Set(nil) = %p, want the process default %p", got, slog.Default())
	}
}
