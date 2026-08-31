package timesync

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// TestServiceStopJoinsLoop locks in C4's fix: stop() must not return until
// loop() has actually exited, so a retired Service can never still be
// mid-sync (and therefore able to call setSystemTime) by the time Stop or
// Restart returns and a replacement Service has already synced. Before this
// fix, stop() only closed a signal channel and returned immediately, leaving
// the old loop's in-flight sync (up to ~15-20s worst case) free to run on.
//
// A real loop()/sync() pumps actual RTC and network I/O, so this stands in a
// fake loop goroutine that mirrors the one contract under test: it exits only
// after observing ctx cancellation, and closes loopDone on the way out.
func TestServiceStopJoinsLoop(t *testing.T) {
	sctx, scancel := context.WithCancel(context.Background())
	s := &Service{
		ctx:      sctx,
		cancel:   scancel,
		loopDone: make(chan struct{}),
		log:      slog.New(slog.DiscardHandler),
	}

	unblockLoop := make(chan struct{})
	loopExited := make(chan struct{})
	go func() {
		defer close(s.loopDone)
		<-s.ctx.Done() // only reachable once stop() calls s.cancel()
		<-unblockLoop  // stands in for sync() still running a moment longer
		close(loopExited)
	}()

	stopReturned := make(chan struct{})
	go func() {
		s.stop()
		close(stopReturned)
	}()

	// stop() must block while the loop is still running -- this is the JOIN
	// C4 added; the old implementation would let stopReturned close here.
	select {
	case <-stopReturned:
		t.Fatal("stop() returned before the loop exited")
	case <-time.After(50 * time.Millisecond):
	}

	close(unblockLoop)

	select {
	case <-loopExited:
	case <-time.After(time.Second):
		t.Fatal("fake loop never exited (stop() did not cancel the Service's ctx)")
	}
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("stop() did not return promptly after the loop exited")
	}
}

// TestServiceStopIdempotent guards stopOnce: a second stop() (Stop/Restart
// calling into an already-stopped Service) must not panic on a double
// channel close or block forever on a loopDone that will never close twice.
func TestServiceStopIdempotent(t *testing.T) {
	sctx, scancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	close(loopDone) // loop already exited

	s := &Service{
		ctx:      sctx,
		cancel:   scancel,
		loopDone: loopDone,
		log:      slog.New(slog.DiscardHandler),
	}

	done := make(chan struct{})
	go func() {
		s.stop()
		s.stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second stop() call did not return")
	}
}
