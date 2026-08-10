package power

import (
	"errors"
	"testing"
	"time"

	"github.com/warthog618/go-gpiocdev"
	"github.com/warthog618/go-gpiosim"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// ledOffset is the simulated line standing in for the power-LED pin.
const ledOffset = 4

// newSimController spins up a gpio-sim chip, points the power-LED config at it,
// and returns a fresh (non-singleton) controller plus the sim to drive the line.
//
// gpio-sim needs the module loaded and configfs writable, i.e. root. Skip rather
// than fail when that isn't available, so the suite stays green in a plain
// container.
func newSimController(t *testing.T) (*Controller, *gpiosim.Simpleton) {
	t.Helper()

	sim, err := gpiosim.NewSimpleton(8)
	if err != nil {
		t.Skipf("gpio-sim unavailable (needs root + gpio-sim module): %v", err)
	}
	t.Cleanup(sim.Close)

	c := &Controller{
		gpioPowerLED: config.GPIOPin{Chip: sim.ChipName(), Line: ledOffset},
		lines:        make(map[config.GPIOPin]*gpiocdev.Line),
		subs:         make(map[chan bool]struct{}),
	}
	t.Cleanup(func() {
		if c.ledLine != nil {
			_ = c.ledLine.Close()
		}
	})
	return c, sim
}

// setLED drives the simulated LED line and waits for the controller's cache to
// catch up, since edge delivery is asynchronous.
func setLED(t *testing.T, c *Controller, sim *gpiosim.Simpleton, level int) {
	t.Helper()
	if err := sim.SetPull(ledOffset, level); err != nil {
		t.Fatalf("drive LED line to %d: %v", level, err)
	}
	waitState(t, c, level == 1)
}

func waitState(t *testing.T, c *Controller, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := c.State()
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("power state never became %v", want)
}

// TestStateSeedsFromLineLevel checks that the first State call adopts whatever
// level the LED line already sits at, rather than defaulting to off.
func TestStateSeedsFromLineLevel(t *testing.T) {
	c, sim := newSimController(t)

	if err := sim.SetPull(ledOffset, 1); err != nil {
		t.Fatalf("pull LED high: %v", err)
	}

	on, err := c.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !on {
		t.Fatal("State = off, want on: watcher did not seed from the current line level")
	}
}

// TestStateTracksEdges is the core of the edge-driven design: no polling, the
// cache follows the line.
func TestStateTracksEdges(t *testing.T) {
	c, sim := newSimController(t)

	setLED(t, c, sim, 0)
	setLED(t, c, sim, 1)
	setLED(t, c, sim, 0)
	setLED(t, c, sim, 1)
}

// TestWatchDeliversTransitions checks the Watch fan-out that SSE rides on.
func TestWatchDeliversTransitions(t *testing.T) {
	c, sim := newSimController(t)
	setLED(t, c, sim, 0)

	ch, cancel, err := c.Watch()
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	if err := sim.SetPull(ledOffset, 1); err != nil {
		t.Fatalf("pull LED high: %v", err)
	}

	select {
	case on, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed")
		}
		if !on {
			t.Fatal("got off on rising edge, want on")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered on rising edge")
	}
}

// TestWatchCancelIsIdempotent guards the SSE handler's deferred cancel, which
// can race with a stream that already returned.
func TestWatchCancelIsIdempotent(t *testing.T) {
	c, _ := newSimController(t)

	_, cancel, err := c.Watch()
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	cancel() // must not panic on double close
}

// TestSlowSubscriberDoesNotBlockEvents pins the non-blocking fan-out: a
// subscriber that never reads must not stall the gpiocdev event goroutine, and
// must still observe the latest value rather than a stale backlog.
func TestSlowSubscriberDoesNotBlockEvents(t *testing.T) {
	c, sim := newSimController(t)
	setLED(t, c, sim, 0)

	ch, cancel, err := c.Watch()
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	// Far more transitions than the channel's buffer of 1, none of them read.
	for i := 0; i < 20; i++ {
		if err := sim.SetPull(ledOffset, (i+1)%2); err != nil {
			t.Fatalf("toggle LED: %v", err)
		}
	}

	// The controller must still be live and tracking, not deadlocked behind the
	// unread subscriber.
	setLED(t, c, sim, 1)

	// And the subscriber still has a value waiting rather than being starved.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber starved: fan-out is blocking")
	}
}

// TestStateDoesNotBlockOnPowerSequence is the regression test for the bug this
// change fixes: State used to take the controller mutex, so it queued behind a
// multi-second power sequence.
func TestStateDoesNotBlockOnPowerSequence(t *testing.T) {
	c, sim := newSimController(t)
	setLED(t, c, sim, 1)

	// Simulate a power sequence holding the controller mutex.
	c.mu.Lock()
	defer c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.State(); err != nil {
			t.Errorf("State during power sequence: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("State blocked on the power-sequence mutex")
	}
}

// TestWaitForOffReturnsOnFallingEdge covers the Reset path, which now blocks on
// an edge instead of polling.
func TestWaitForOffReturnsOnFallingEdge(t *testing.T) {
	c, sim := newSimController(t)
	setLED(t, c, sim, 1)

	done := make(chan error, 1)
	go func() { done <- c.waitForOff() }()

	// Give waitForOff time to subscribe before the edge lands.
	time.Sleep(50 * time.Millisecond)
	if err := sim.SetPull(ledOffset, 0); err != nil {
		t.Fatalf("drop LED low: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForOff: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForOff did not return on the falling edge")
	}
}

// TestWaitForOffReturnsImmediatelyWhenAlreadyOff exercises the check-after-
// subscribe ordering: no edge will ever arrive, so a missed pre-check hangs.
func TestWaitForOffReturnsImmediatelyWhenAlreadyOff(t *testing.T) {
	c, sim := newSimController(t)
	setLED(t, c, sim, 0)

	done := make(chan error, 1)
	go func() { done <- c.waitForOff() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForOff: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitForOff hung although power was already off")
	}
}

// TestWatchRejectsLegacyMode: legacy has no LED line, so SSE must fall back to
// polling rather than silently never firing.
func TestWatchRejectsLegacyMode(t *testing.T) {
	c, _ := newSimController(t)
	c.legacyMode = true

	if _, _, err := c.Watch(); !errors.Is(err, ErrNoEdgeEvents) {
		t.Fatalf("Watch in legacy mode = %v, want ErrNoEdgeEvents", err)
	}
}

// TestRpibootRejectsLegacyMode: the rpiboot combination is a held button
// press, which legacy direct-rail mode cannot express — it must refuse rather
// than glitch the supply. The guard fires before any GPIO access, so no sim
// chip is needed.
func TestRpibootRejectsLegacyMode(t *testing.T) {
	c := &Controller{
		legacyMode: true,
		lines:      make(map[config.GPIOPin]*gpiocdev.Line),
		subs:       make(map[chan bool]struct{}),
	}
	if err := c.Rpiboot(); err == nil {
		t.Fatal("Rpiboot in legacy mode succeeded, want error")
	}
}
