package network

import (
	"log/slog"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// discardLog is a component logger that discards everything, for tests that
// build a Manager directly and exercise a method that may log.
func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestHasCarrier(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs *netlink.LinkAttrs
		want  bool
	}{
		{"IFF_RUNNING set", &netlink.LinkAttrs{RawFlags: unix.IFF_RUNNING}, true},
		// Some drivers leave operstate unknown but still raise IFF_RUNNING, and
		// others do the reverse; either source alone must report the carrier.
		{"operstate up only", &netlink.LinkAttrs{OperState: netlink.OperUp}, true},
		{"admin up but no carrier", &netlink.LinkAttrs{RawFlags: unix.IFF_UP}, false},
		{"lower layer down", &netlink.LinkAttrs{OperState: netlink.OperLowerLayerDown}, false},
		{"nil attrs", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCarrier(tc.attrs); got != tc.want {
				t.Errorf("hasCarrier = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDHCPMode(t *testing.T) {
	// Must mirror superviseEth0's switch: only an explicit "static" is static.
	for mode, want := range map[string]bool{
		"":         true,
		"dhcp":     true,
		"DHCP":     true,
		"static":   false,
		"Static":   false,
		"nonsense": true,
	} {
		if got := dhcpMode(mode); got != want {
			t.Errorf("dhcpMode(%q) = %v, want %v", mode, got, want)
		}
	}
}

func newCarrierTestManager(mode string) *Manager {
	m := &Manager{dhcpReacquire: make(chan struct{}, 1), log: discardLog()}
	m.cfg.Eth0 = config.InterfaceConfig{Name: "eth0", Mode: mode}
	return m
}

// signalled drains the reacquire channel, reporting whether one was pending.
func signalled(m *Manager) bool {
	select {
	case <-m.dhcpReacquire:
		return true
	default:
		return false
	}
}

func TestNoteEth0CarrierSignalsOnRisingEdgeOnly(t *testing.T) {
	m := newCarrierTestManager("dhcp")

	// Starting state is carrier-down, so the first up is an edge.
	m.noteEth0Carrier(true)
	if !signalled(m) {
		t.Fatal("no reacquire raised when the carrier first came up")
	}

	// Staying up must not keep re-triggering: unrelated netlink events on a
	// healthy link would otherwise restart DHCP continuously.
	m.noteEth0Carrier(true)
	if signalled(m) {
		t.Error("reacquire raised while the carrier stayed up")
	}

	// Down is not an edge either; only the return to carrier is.
	m.noteEth0Carrier(false)
	if signalled(m) {
		t.Error("reacquire raised when the carrier went down")
	}

	m.noteEth0Carrier(true)
	if !signalled(m) {
		t.Error("no reacquire raised when the carrier came back")
	}
}

func TestNoteEth0CarrierStaticModeDoesNotSignal(t *testing.T) {
	m := newCarrierTestManager(ModeStatic)
	m.noteEth0Carrier(true)
	if signalled(m) {
		t.Error("reacquire raised for a statically addressed interface")
	}
}

func TestAwaitEth0ReturnsWhenEth0Attempted(t *testing.T) {
	m := &Manager{eth0Ready: make(chan struct{}), log: discardLog()}
	m.cfg.Eth0 = config.InterfaceConfig{Name: "eth0", Mode: "dhcp"}
	m.cfg.RHI = config.RHIConfig{Interface: "usb0"}

	// eth0Ready closes on the first *attempt*, success or not; that is the
	// signal the RHI waits on.
	m.signalEth0Ready()

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() { m.awaitEth0(done); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitEth0 did not return once eth0 had been attempted")
	}
}

func TestAwaitEth0GivesUpAtTheCap(t *testing.T) {
	// A broken or unplugged uplink must delay the RHI by the cap and no more:
	// the RHI is the access path that has to survive a dead eth0.
	old := rhiEth0Wait
	rhiEth0Wait = 40 * time.Millisecond
	defer func() { rhiEth0Wait = old }()

	m := &Manager{eth0Ready: make(chan struct{}), log: discardLog()} // never closed
	m.cfg.Eth0 = config.InterfaceConfig{Name: "eth0", Mode: "dhcp"}
	m.cfg.RHI = config.RHIConfig{Interface: "usb0"}

	start := time.Now()
	m.awaitEth0(make(chan struct{}))
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("returned after %s, expected to wait out the %s cap", elapsed, rhiEth0Wait)
	} else if elapsed > 2*time.Second {
		t.Errorf("waited %s, far beyond the cap", elapsed)
	}
}

func TestAwaitEth0ReturnsOnShutdown(t *testing.T) {
	old := rhiEth0Wait
	rhiEth0Wait = time.Hour // would hang if shutdown were not honoured
	defer func() { rhiEth0Wait = old }()

	m := &Manager{eth0Ready: make(chan struct{}), log: discardLog()}
	m.cfg.Eth0 = config.InterfaceConfig{Name: "eth0"}
	done := make(chan struct{})
	close(done)

	finished := make(chan struct{})
	go func() { m.awaitEth0(done); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitEth0 ignored shutdown")
	}
}

func TestAwaitEth0NoOpWhenEth0Unconfigured(t *testing.T) {
	old := rhiEth0Wait
	rhiEth0Wait = time.Hour
	defer func() { rhiEth0Wait = old }()

	// With no uplink configured there is nothing to defer to, so the RHI must
	// not pay the wait at all.
	m := &Manager{eth0Ready: make(chan struct{}), log: discardLog()}
	finished := make(chan struct{})
	go func() { m.awaitEth0(make(chan struct{})); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitEth0 waited despite eth0 not being configured")
	}
}

func TestNoteEth0CarrierCoalescesSignals(t *testing.T) {
	// The channel holds one; a second edge with one already pending must not
	// block the link monitor.
	m := newCarrierTestManager("dhcp")
	m.noteEth0Carrier(true)
	m.noteEth0Carrier(false)
	m.noteEth0Carrier(true)

	if !signalled(m) {
		t.Fatal("expected a pending reacquire")
	}
	if signalled(m) {
		t.Error("expected the pending reacquires to coalesce into one")
	}
}

func TestGrowBackoff(t *testing.T) {
	if got := growBackoff(supRetryFloor); got != 2*supRetryFloor {
		t.Errorf("growBackoff(floor) = %s, want %s", got, 2*supRetryFloor)
	}
	if got := growBackoff(supRetryCap); got != supRetryCap {
		t.Errorf("growBackoff(cap) = %s, want %s (capped)", got, supRetryCap)
	}
	if got := growBackoff(45 * time.Second); got != supRetryCap {
		t.Errorf("growBackoff(45s) = %s, want cap %s", got, supRetryCap)
	}
}

// A link that was already up before the monitor started must not look like a
// rising edge to the first netlink event it sees.
//
// This is what put two full DHCP exchanges on the wire in the same second, on
// every start-up: eth0Carrier begins false, the first event reports the
// carrier that had been up all along, noteEth0Carrier calls that a rise, and
// the runner throws away the lease it had just bound to go and DISCOVER again.
func TestSeededCarrierSuppressesTheFalseRisingEdge(t *testing.T) {
	m := newCarrierTestManager("dhcp")

	// What seedEth0Carrier does for a link that is already up.
	m.mu.Lock()
	m.eth0Carrier = true
	m.mu.Unlock()

	m.noteEth0Carrier(true)
	if signalled(m) {
		t.Error("an already-up carrier was reported as a rising edge; the lease would be discarded and re-DISCOVERed")
	}

	// A genuine flap must still be seen.
	m.noteEth0Carrier(false)
	m.noteEth0Carrier(true)
	if !signalled(m) {
		t.Error("a real down/up flap did not signal a reacquire")
	}
}

// seedEth0Carrier reads the live link rather than assuming. Loopback is always
// up, so it is a usable stand-in for "the link was up before we looked".
func TestSeedEth0CarrierReadsTheLiveLink(t *testing.T) {
	if _, err := netlink.LinkByName("lo"); err != nil {
		t.Skipf("no loopback link here: %v", err)
	}
	m := newCarrierTestManager("dhcp")
	m.cfg.Eth0.Name = "lo"

	m.seedEth0Carrier()

	m.mu.Lock()
	got := m.eth0Carrier
	m.mu.Unlock()
	if !got {
		t.Error("seedEth0Carrier did not record the carrier of an up link")
	}
	if signalled(m) {
		t.Error("seeding must not signal a reacquire; it only primes the edge detector")
	}
}

// An interface that does not exist leaves the zero value, which is the right
// starting point: there is no carrier to have missed.
func TestSeedEth0CarrierToleratesAMissingLink(t *testing.T) {
	m := newCarrierTestManager("dhcp")
	m.cfg.Eth0.Name = "nosuchif0"

	m.seedEth0Carrier()

	m.mu.Lock()
	got := m.eth0Carrier
	m.mu.Unlock()
	if got {
		t.Error("a missing link was recorded as having carrier")
	}
}
