// Package power provides GPIO power management for the BMC.
//
// # Button-press mode (preferred)
//
// When the power-LED GPIO (GPIOPowerLED) is configured and readable, the
// controller operates in button-press mode:
//   - Power state is tracked from edge events on the LED pin (1 = on, 0 = off),
//     not by polling. See "Edge-driven state" below.
//   - The power-button GPIO (GPIOPower) is driven open-drain, matching a real
//     momentary button: the host holds it HIGH via an external pull-up, and a
//     press shorts it to ground. Releasing leaves the line high-impedance so the
//     host restores HIGH — the BMC never actively drives it high.
//   - A press is simulated by pulling the pin to ground (HIGH→LOW) for a
//     duration, then releasing (float back to HIGH).
//   - Short press (~300 ms): power-on or graceful/soft shutdown.
//   - Long press  (≥5 s):   forced hard shutdown (ATX-style hold).
//   - Held press through power-on (~3 s from standby): forces the Raspberry
//     Pi 5 BootROM into rpiboot (USB device) mode — see Rpiboot.
//
// # Legacy / fallback mode
//
// When GPIOPowerLED is not configured or unreadable the controller falls back
// to direct GPIO control (original behaviour):
//   - State is read from the GPIOPower pin itself (1 = on, 0 = off).
//   - Power-on uses a 1→0→1 toggle sequence to trigger boot.
//   - Power-off / force-off set the pin directly to 0 (immediate cut).
//
// # Edge-driven state
//
// In button-press mode the LED line is requested once with both-edge detection.
// The kernel delivers an event on every transition; the handler updates a cached
// atomic and fans the new value out to subscribers registered via Watch. State
// therefore never touches hardware and never blocks — important because a long
// power sequence (ForceOff, Reset) holds the controller mutex for many seconds
// while it waits for the host to go down, and callers polling State would
// otherwise queue up behind it.
//
// Legacy mode has no LED line, so State reads the button pin directly under the
// mutex and Watch reports ErrNoEdgeEvents.
//
// Power *actions* are serialized via a mutex; State and Watch are not.
package power

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/warthog618/go-gpiocdev"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
)

// ErrNoEdgeEvents is returned by Watch when the controller cannot deliver
// change notifications — currently only in legacy mode, which has no LED line.
// Callers should fall back to polling State.
var ErrNoEdgeEvents = errors.New("power: edge events unavailable (legacy mode)")

// gpioConsumer labels the process holding the GPIO lines (shown in gpioinfo).
const gpioConsumer = "nanokvm-power"

const (
	// shortPressDuration is the hold time for power-on and graceful shutdown.
	shortPressDuration = 300 * time.Millisecond

	// longPressDuration is the hold time for a forced power-off (ATX spec ≥4 s).
	longPressDuration = 5500 * time.Millisecond

	// rpibootHoldDuration is how long the power button stays held to force the
	// Raspberry Pi 5 BootROM into rpiboot mode. The press wakes the PMIC from
	// standby and the BootROM samples the button within the first moments of
	// boot, so a few seconds is plenty; it is kept well under the ≥4 s
	// forced-off threshold in case the host turns out to be running normally.
	rpibootHoldDuration = 3 * time.Second

	// toggleDelay is the pause between steps in the legacy boot sequence.
	toggleDelay = 200 * time.Millisecond

	// postPressDelay lets the hardware settle after releasing the button.
	postPressDelay = 500 * time.Millisecond

	// offTimeout bounds how long waitForOff blocks for the host to power down.
	offTimeout = 30 * time.Second

	// ledDebounce filters contact/level noise on the power-LED line. It must
	// stay well below the blink period of a pulsing standby LED, whose real
	// transitions we still want to see.
	ledDebounce = 10 * time.Millisecond
)

// Controller manages system power via GPIO. All public methods are serialized
// via a mutex to prevent concurrent GPIO sequences.
//
// GPIO lines are accessed through the character-device interface
// (CONFIG_GPIO_CDEV). Each line is requested once, lazily, and the request is
// held open for the process lifetime (lines map). Holding the request open is
// what keeps an output line driving its level after a call returns — closing it
// would release the line and drop the drive, which matters for legacy
// direct-drive power-off (pin must stay LOW). The kernel reclaims the lines when
// the process exits. Access to lines is serialized by mu together with the rest
// of the controller.
//
// The power-LED line is deliberately NOT part of lines: it is held under ledMu
// with its own lazy request, so reading power state never contends with a power
// sequence holding mu. Lock order is mu → ledMu; nothing takes them the other
// way round.
type Controller struct {
	// legacyMode and the GPIO pins are fixed at construction from config —
	// set once by NewController, never re-read, so a controller's behaviour
	// can't shift under a caller mid-request.
	legacyMode   bool
	gpioPower    config.GPIOPin
	gpioPowerLED config.GPIOPin

	mu    sync.Mutex
	lines map[config.GPIOPin]*gpiocdev.Line

	// ledMu guards ledLine. ledOn is the edge-maintained cache of the LED level
	// and is read without any lock.
	ledMu   sync.Mutex
	ledLine *gpiocdev.Line
	ledOn   atomic.Bool

	// subsMu guards subs, the set of Watch channels. The gpiocdev event
	// goroutine publishes into them; it must never block, so sends are
	// non-blocking with latest-value semantics.
	subsMu sync.Mutex
	subs   map[chan bool]struct{}
}

// NewController builds a power Controller from its config sections. Called
// once by cmd/server/main.go; the returned Controller is then threaded to
// every consumer via pkg/deps instead of a package-level singleton.
func NewController(hw config.Hardware, pw config.Power) *Controller {
	return &Controller{
		legacyMode:   pw.LegacyMode,
		gpioPower:    hw.GPIOPower,
		gpioPowerLED: hw.GPIOPowerLED,
		lines:        make(map[config.GPIOPin]*gpiocdev.Line),
		subs:         make(map[chan bool]struct{}),
	}
}

// ── Public API ────────────────────────────────────────────────────────────────

// State reports whether the system is powered on.
//
// Button mode: returns the edge-maintained cache — no ioctl, no lock, so it
// stays responsive while a multi-second power sequence holds mu.
// Legacy mode:  reads the button pin directly under mu.
func (c *Controller) State() (bool, error) {
	if !c.legacyMode {
		if err := c.ensureLEDWatcher(); err != nil {
			return false, err
		}
		return c.ledOn.Load(), nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	on, err := c.readState()
	if err == nil {
		telemetry.PowerState(context.Background(), on)
	}
	return on, err
}

// Watch returns a channel that receives the power state on every change, plus a
// cancel func the caller must invoke to unsubscribe (it closes the channel).
//
// The channel has latest-value semantics: a subscriber that falls behind sees
// the most recent state, never a backlog. It does NOT receive the current state
// on subscribe — call State for that, before or after Watch (a duplicate is
// harmless; a missed transition is not).
//
// Returns ErrNoEdgeEvents in legacy mode, where the caller must poll State.
func (c *Controller) Watch() (<-chan bool, func(), error) {
	if c.legacyMode {
		return nil, nil, ErrNoEdgeEvents
	}
	if err := c.ensureLEDWatcher(); err != nil {
		return nil, nil, err
	}

	ch := make(chan bool, 1)
	c.subsMu.Lock()
	c.subs[ch] = struct{}{}
	c.subsMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			c.subsMu.Lock()
			delete(c.subs, ch)
			close(ch)
			c.subsMu.Unlock()
		})
	}
	return ch, cancel, nil
}

// PowerOn powers on the system.
//
// Button mode:  sends a short press if the system is currently off.
// Legacy mode:  runs the 1→0→1 boot sequence on the power pin.
func (c *Controller) PowerOn() (retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { telemetry.PowerOperation(context.Background(), "on", retErr) }()

	on, err := c.readState()
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if on {
		log.Debug("power: already on, no-op")
		return nil
	}

	if !c.legacyMode {
		log.Info("power: power-on (short button press)")
		return c.buttonPress(shortPressDuration)
	}

	log.Info("power: power-on boot sequence (legacy mode)")
	return c.legacyBootSequence()
}

// PowerOff sends a graceful/soft shutdown signal.
//
// Button mode:  short button press (OS handles the ACPI shutdown).
// Legacy mode:  sets the power pin to 0 (immediate cut).
func (c *Controller) PowerOff() (retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { telemetry.PowerOperation(context.Background(), "off", retErr) }()

	on, err := c.readState()
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if !on {
		log.Debug("power: already off, no-op")
		return nil
	}

	if !c.legacyMode {
		log.Info("power: soft shutdown (short button press)")
		return c.buttonPress(shortPressDuration)
	}

	log.Info("power: power-off (legacy — set pin 0)")
	return c.legacyWritePin(0)
}

// ForceOff forces an immediate hard shutdown.
//
// Button mode:  holds the power button LOW for ≥5 s.
// Legacy mode:  sets the power pin to 0 (same as PowerOff — immediate cut).
func (c *Controller) ForceOff() (retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { telemetry.PowerOperation(context.Background(), "force_off", retErr) }()

	on, err := c.readState()
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if !on {
		log.Debug("power: already off, no-op")
		return nil
	}

	if !c.legacyMode {
		log.Info("power: force shutdown (long button press ≥5 s)")
		return c.buttonPress(longPressDuration)
	}

	log.Info("power: force-off (legacy — set pin 0)")
	return c.legacyWritePin(0)
}

// Reset forces the system off and powers it back on.
func (c *Controller) Reset() (retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { telemetry.PowerOperation(context.Background(), "reset", retErr) }()

	on, err := c.readState()
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}

	if on {
		if !c.legacyMode {
			log.Info("power: reset — force off (long button press)")
			if err := c.buttonPress(longPressDuration); err != nil {
				return fmt.Errorf("reset force-off: %w", err)
			}
			if err := c.waitForOff(); err != nil {
				log.Warnf("power: reset — timed out waiting for off, proceeding anyway: %v", err)
			}
		} else {
			log.Info("power: reset — off (legacy)")
			if err := c.legacyWritePin(0); err != nil {
				return fmt.Errorf("reset legacy off: %w", err)
			}
			time.Sleep(toggleDelay)
		}
	}

	if !c.legacyMode {
		log.Info("power: reset — power on (short press)")
		return c.buttonPress(shortPressDuration)
	}

	log.Info("power: reset — boot sequence (legacy)")
	return c.legacyBootSequence()
}

// Rpiboot forces the Raspberry Pi 5 into rpiboot (BootROM USB device) mode,
// where it enumerates as VID:PID 0a5c:2712 and waits for a payload over USB
// (see cmd/rpiboot).
//
// The documented combination is "hold the power button whilst applying
// power". The BMC cannot cut the 5 V rail, but the same combination is
// reachable from PMIC standby: pressing the button is itself what wakes the
// PMIC, so a press held through the resulting power-on is sampled by the
// BootROM exactly like a press held while inserting the supply.
//
// Sequence: if the host is running, force it off first (long press, then wait
// for the power LED to drop), then press and hold the button for
// rpibootHoldDuration and release.
//
// Button-press mode only: legacy mode drives the supply rail directly and has
// no button to hold, so there is no combination to send.
func (c *Controller) Rpiboot() (retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { telemetry.PowerOperation(context.Background(), "rpiboot", retErr) }()

	if c.legacyMode {
		return fmt.Errorf("rpiboot requires button-press mode (legacy mode has no power button to hold)")
	}

	on, err := c.readState()
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if on {
		log.Info("power: rpiboot — force off (long button press)")
		if err := c.buttonPress(longPressDuration); err != nil {
			return fmt.Errorf("rpiboot force-off: %w", err)
		}
		if err := c.waitForOff(); err != nil {
			log.Warnf("power: rpiboot — timed out waiting for off, proceeding anyway: %v", err)
		}
	}

	log.Info("power: rpiboot — holding power button through power-on")
	return c.buttonPress(rpibootHoldDuration)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// readState returns true if the system is powered on.
// Button mode: returns the edge-maintained LED cache.
// Legacy mode:  reads the button/power pin directly.
// Caller must hold c.mu.
func (c *Controller) readState() (bool, error) {
	if !c.legacyMode {
		if err := c.ensureLEDWatcher(); err != nil {
			return false, err
		}
		return c.ledOn.Load(), nil
	}

	// Legacy mode drives the power pin as output; reading it back reports the
	// level we last drove.
	v, err := c.readOutput(c.gpioPower)
	if err != nil {
		return false, fmt.Errorf("read power pin: %w", err)
	}
	return v == 1, nil
}

// ── Power-LED edge watcher ────────────────────────────────────────────────────

// ensureLEDWatcher lazily requests the power-LED line with both-edge detection
// and seeds ledOn with its current level. Safe to call concurrently and from any
// lock context (it only takes ledMu). Idempotent once the line is held.
func (c *Controller) ensureLEDWatcher() error {
	c.ledMu.Lock()
	defer c.ledMu.Unlock()

	if c.ledLine != nil {
		return nil
	}

	pin := c.gpioPowerLED
	if pin.IsZero() {
		return fmt.Errorf("GPIOPowerLED not configured (required for button-press mode)")
	}

	opts := []gpiocdev.LineReqOption{
		gpiocdev.AsInput,
		gpiocdev.WithBothEdges,
		gpiocdev.WithEventHandler(c.onLEDEvent),
		gpiocdev.WithConsumer(gpioConsumer),
	}

	// Hardware debounce needs GPIO uAPI v2. Kernels stuck on v1 reject the
	// request outright, so retry without it — edge events still work, we just
	// see the noise.
	line, err := gpiocdev.RequestLine(pin.Chip, pin.Line, append(opts, gpiocdev.WithDebounce(ledDebounce))...)
	if err != nil {
		log.Debugf("power: %s debounce unavailable (%v), retrying without", pin, err)
		line, err = gpiocdev.RequestLine(pin.Chip, pin.Line, opts...)
		if err != nil {
			return fmt.Errorf("watch power-LED pin %s: %w", pin, err)
		}
	}

	// Seed from the current level. Any edge that lands between the request and
	// this read is delivered to the handler, so no transition is lost.
	v, err := line.Value()
	if err != nil {
		_ = line.Close()
		return fmt.Errorf("read power-LED pin %s: %w", pin, err)
	}

	c.ledLine = line
	c.setLED(v == 1)
	log.Infof("power: watching power-LED %s (initial state: %v)", pin, v == 1)
	return nil
}

// onLEDEvent runs on the gpiocdev event goroutine. It must not block.
func (c *Controller) onLEDEvent(evt gpiocdev.LineEvent) {
	c.setLED(evt.Type == gpiocdev.LineEventRisingEdge)
}

// setLED updates the cached level and, on a change, records telemetry and fans
// the new value out to subscribers.
func (c *Controller) setLED(on bool) {
	if c.ledOn.Swap(on) == on {
		return
	}
	log.Debugf("power: power-LED changed to %v", on)
	telemetry.PowerState(context.Background(), on)

	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for ch := range c.subs {
		// Latest-value: drop any stale queued value, then enqueue this one.
		// Both sends are non-blocking, so a wedged subscriber can't stall the
		// event goroutine.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- on:
		default:
		}
	}
}

// buttonPress simulates a button press: ensures the pin is HIGH, pulls it LOW
// for duration, then restores HIGH. The pin is always left at 1 (released).
// Caller must hold c.mu.
func (c *Controller) buttonPress(duration time.Duration) error {
	pin := c.gpioPower
	if pin.IsZero() {
		return fmt.Errorf("GPIOPower not configured")
	}

	line, err := c.buttonLine(pin)
	if err != nil {
		return err
	}

	// Guarantee starting state is released (high-impedance, host holds HIGH).
	if err := line.SetValue(1); err != nil {
		return fmt.Errorf("pre-press release: %w", err)
	}

	// Pull to ground — button pressed (open-drain actively drives LOW).
	if err := line.SetValue(0); err != nil {
		return fmt.Errorf("press down: %w", err)
	}
	log.Debugf("power: gpio %s pressed (pulled to ground)", pin)
	time.Sleep(duration)

	// Release — float back to HIGH (open-drain high-impedance).
	if err := line.SetValue(1); err != nil {
		return fmt.Errorf("press release: %w", err)
	}
	log.Debugf("power: gpio %s released (floating high)", pin)

	time.Sleep(postPressDelay)
	return nil
}

// waitForOff blocks until the power LED reports off, or until offTimeout.
// Caller must hold c.mu — State and Watch do not take it, so concurrent readers
// stay responsive for the whole wait.
func (c *Controller) waitForOff() error {
	ch, cancel, err := c.Watch()
	if err != nil {
		return err
	}
	defer cancel()

	// Subscribe first, then check: an edge that lands in between is queued on
	// ch rather than missed.
	if !c.ledOn.Load() {
		return nil
	}

	timeout := time.NewTimer(offTimeout)
	defer timeout.Stop()

	for {
		select {
		case on, ok := <-ch:
			if !ok {
				return fmt.Errorf("power-LED watch closed while waiting for power off")
			}
			if !on {
				return nil
			}
		case <-timeout.C:
			return fmt.Errorf("timed out after %s waiting for power off", offTimeout)
		}
	}
}

// ── Legacy helpers ────────────────────────────────────────────────────────────

// legacyBootSequence performs the 1→0→1 toggle that triggers boot when the
// power pin is wired directly to the power supply enable.
// Caller must hold c.mu.
func (c *Controller) legacyBootSequence() error {
	pin := c.gpioPower
	for _, v := range []int{1, 0, 1} {
		if err := c.writeOutput(pin, v); err != nil {
			return fmt.Errorf("boot sequence (write %d): %w", v, err)
		}
		time.Sleep(toggleDelay)
	}
	return nil
}

// legacyWritePin sets the power pin directly (0 = off, 1 = on).
// Caller must hold c.mu.
func (c *Controller) legacyWritePin(val int) error {
	pin := c.gpioPower
	if err := c.writeOutput(pin, val); err != nil {
		return fmt.Errorf("write power pin %d: %w", val, err)
	}
	return nil
}

// ── Low-level GPIO access (character device, CONFIG_GPIO_CDEV) ─────────────────
//
// Lines are requested lazily and cached in c.lines; the request is held open so
// output levels persist after a call returns. All helpers require c.mu held,
// which the public API guarantees.

// outputLine returns a cached request for pin configured as output. On first
// use the line is sampled as input and then reconfigured to output at that same
// level, so acquiring the line does not glitch the current pin state (important
// so starting the daemon never toggles power).
func (c *Controller) outputLine(pin config.GPIOPin) (*gpiocdev.Line, error) {
	if l, ok := c.lines[pin]; ok {
		return l, nil
	}
	l, err := gpiocdev.RequestLine(pin.Chip, pin.Line, gpiocdev.AsInput, gpiocdev.WithConsumer(gpioConsumer))
	if err != nil {
		return nil, fmt.Errorf("request %s as input: %w", pin, err)
	}
	cur, err := l.Value()
	if err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("sample %s: %w", pin, err)
	}
	if err := l.Reconfigure(gpiocdev.AsOutput(cur)); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("reconfigure %s as output: %w", pin, err)
	}
	c.lines[pin] = l
	return l, nil
}

// buttonLine returns a cached open-drain request for the power-button pin.
//
// A momentary power button is active-low: the host holds it HIGH through an
// external pull-up, and a press shorts it to ground. Open-drain drive reproduces
// this exactly — value 0 pulls the line to ground (pressed); value 1 leaves it
// high-impedance (released) so the host pull-up restores HIGH. The BMC never
// actively sources HIGH, so it can't contend with the host's button circuit.
// gpiolib emulates open-drain on chips without native support, so this works
// regardless of the SoC. The internal pull-up guarantees the released line
// settles HIGH even if the host's external pull-up is weak or absent. The
// initial value is released (high-impedance), which cannot disturb the host, so
// no glitch-free sampling is needed here.
func (c *Controller) buttonLine(pin config.GPIOPin) (*gpiocdev.Line, error) {
	if l, ok := c.lines[pin]; ok {
		return l, nil
	}
	l, err := gpiocdev.RequestLine(pin.Chip, pin.Line,
		gpiocdev.AsOutput(1),
		gpiocdev.AsOpenDrain,
		gpiocdev.WithPullUp,
		gpiocdev.WithConsumer(gpioConsumer))
	if err != nil {
		return nil, fmt.Errorf("request %s as open-drain output: %w", pin, err)
	}
	c.lines[pin] = l
	return l, nil
}

// readOutput reads back the level currently driven on an output pin (0 or 1).
func (c *Controller) readOutput(pin config.GPIOPin) (int, error) {
	l, err := c.outputLine(pin)
	if err != nil {
		return 0, err
	}
	return l.Value()
}

// writeOutput drives an output pin to val (any non-zero value means high). The
// held-open line request keeps the level asserted after this returns.
func (c *Controller) writeOutput(pin config.GPIOPin, val int) error {
	l, err := c.outputLine(pin)
	if err != nil {
		return err
	}
	v := 0
	if val != 0 {
		v = 1
	}
	if err := l.SetValue(v); err != nil {
		return fmt.Errorf("set %s = %d: %w", pin, v, err)
	}
	log.Debugf("power: gpio %s = %d", pin, v)
	return nil
}
