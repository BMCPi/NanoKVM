package vm

import (
	"sync"
	"testing"
	"time"
)

// A stalled device write must not block the reader, and pointer moves that
// arrive during the stall must collapse to the newest position while key
// events keep strict order.
func TestHIDApplierCoalescesPointerAndPreservesKeyOrder(t *testing.T) {
	var mu sync.Mutex
	var applied []hidEvent
	release := make(chan struct{})

	a := newHIDApplier(func(ev hidEvent) error {
		<-release // hold "the device write" until the test lets go
		mu.Lock()
		applied = append(applied, ev)
		mu.Unlock()
		return nil
	})
	defer a.stop()

	// Prime both lanes so each is parked inside a stalled write.
	a.enqueue(hidEvent{Type: evKeyDown, Code: 0x04})
	a.enqueue(hidEvent{Type: evAbsMouse, X: 1, Y: 1})
	time.Sleep(20 * time.Millisecond)

	// Flood pointer moves plus one more keystroke while stalled. enqueue must
	// return immediately every time.
	done := make(chan struct{})
	go func() {
		for i := range 500 {
			a.enqueue(hidEvent{Type: evAbsMouse, X: uint16(i), Y: uint16(i), Wheel: 1})
		}
		a.enqueue(hidEvent{Type: evKeyUp, Code: 0x04})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enqueue blocked behind a stalled device write")
	}

	close(release)

	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		if n >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	var keys, pointers []hidEvent
	for _, ev := range applied {
		if ev.Type == evAbsMouse {
			pointers = append(pointers, ev)
		} else {
			keys = append(keys, ev)
		}
	}

	if len(keys) != 2 || keys[0].Type != evKeyDown || keys[1].Type != evKeyUp {
		t.Fatalf("key lane order broken: %+v", keys)
	}
	// The primed move plus at most a couple of drained slots — never 500.
	if len(pointers) > 3 {
		t.Fatalf("pointer flood was not coalesced: %d reports applied", len(pointers))
	}
	last := pointers[len(pointers)-1]
	if last.X != 499 {
		t.Errorf("latest-wins lost the newest position: %+v", last)
	}
	// 500 wheel ticks accumulated and clamped, not dropped with stale moves.
	if last.Wheel != 127 {
		t.Errorf("wheel ticks not accumulated: %d", last.Wheel)
	}
}
