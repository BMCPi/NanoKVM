package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// The bug this guards against is not a slow shutdown, it is a shutdown that
// never returns: closing the video hub joins a supervisor that can be inside
// an ioctl on a wedged driver. When that happened the process stopped exiting
// entirely, kept serving, and ignored every subsequent SIGTERM.
func TestRunBoundedGivesUpOnWorkThatNeverFinishes(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	finished := runBounded(20*time.Millisecond, func() { <-release })
	elapsed := time.Since(start)

	if finished {
		t.Error("runBounded reported success for work that never finished")
	}
	if elapsed > time.Second {
		t.Errorf("runBounded waited %s; it should return at the timeout", elapsed)
	}
}

func TestRunBoundedWaitsForWorkThatFinishes(t *testing.T) {
	var ran atomic.Bool

	if !runBounded(5*time.Second, func() { ran.Store(true) }) {
		t.Error("runBounded reported a timeout for work that completed")
	}
	if !ran.Load() {
		t.Error("runBounded returned before fn ran")
	}
}

// Cleanup runs after the request drain, when nothing is being served, so
// waiting longer there only delays the supervisor restarting a process that
// has already committed to exiting.
func TestDisposeTimeoutIsShorterThanTheDrain(t *testing.T) {
	if disposeTimeout >= shutdownTimeout {
		t.Errorf("disposeTimeout (%s) should be shorter than shutdownTimeout (%s)",
			disposeTimeout, shutdownTimeout)
	}
	if disposeTimeout <= 0 {
		t.Errorf("disposeTimeout is %s; an unbounded dispose is the bug", disposeTimeout)
	}
}
