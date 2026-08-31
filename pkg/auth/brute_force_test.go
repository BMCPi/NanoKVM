package auth

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// TestCleanupRoutineExitsOnCancelledContext is the regression test for I4:
// the brute-force cleanup goroutine used to have no stop path at all (it
// range'd over a ticker forever and never called ticker.Stop()), so it ran
// for the rest of the process's life regardless of the owning Service's
// fate. With rootCtx wired through, a Service built with an
// already-cancelled ctx must let the goroutine exit instead of leaking it.
func TestCleanupRoutineExitsOnCancelledContext(t *testing.T) {
	conf := config.GetInstance()
	saved := conf.Security
	conf.Security.LoginLockoutDuration = 60
	conf.Security.LoginMaxFailures = 5
	t.Cleanup(func() { conf.Security = saved })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the cleanup goroutine ever starts

	s := NewService(ctx, slog.New(slog.DiscardHandler))

	// CheckLoginAttempt (via cleanupOnce) is what actually launches the
	// goroutine; construction alone does not.
	s.CheckLoginAttempt("10.0.0.1")

	select {
	case <-s.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup goroutine did not exit after its context was cancelled")
	}
}

// TestCleanupRoutineRunsUntilContextCancelled is the flip side: with a live
// ctx the goroutine must still be running (not exit immediately for an
// unrelated reason, e.g. a botched select).
func TestCleanupRoutineRunsUntilContextCancelled(t *testing.T) {
	conf := config.GetInstance()
	saved := conf.Security
	conf.Security.LoginLockoutDuration = 60
	conf.Security.LoginMaxFailures = 5
	t.Cleanup(func() { conf.Security = saved })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := NewService(ctx, slog.New(slog.DiscardHandler))
	s.CheckLoginAttempt("10.0.0.1")

	select {
	case <-s.cleanupDone:
		t.Fatal("cleanup goroutine exited before its context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-s.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup goroutine did not exit after cancellation")
	}
}
