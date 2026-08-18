// Package hid drives the two USB HID gadget functions the BMC presents to
// the managed host: the strict boot keyboard (/dev/hidg0, plain 8-byte
// reports so pre-boot firmware can parse them) and the combined pointer
// function (/dev/hidg1: relative mouse + absolute pointer multiplexed with
// Report IDs). pkg/usbgadget/hid.go declares the descriptors.
//
// The keyboard half has to mirror what a real boot-protocol keyboard does,
// because that is what the host's HID stack was told to expect: modifiers are
// bits in byte 0, and ordinary keys occupy a six-slot array reporting which
// keys are down *now* rather than which one just changed. A keystroke is
// therefore a read-modify-write of the whole state, and that state has to live
// somewhere — here. The algorithm (slot reuse, compaction on release, rollover
// on overflow, and the auto-release safety net for key-ups that never arrive)
// follows JetKVM's internal/usbgadget/hid_keyboard.go, which in turn parallels
// the kernel's own hid-gadget behaviour.
package hid

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// signedByte reinterprets a signed report field as the two's-complement byte
// the HID wire format carries. Mouse deltas and wheel ticks are signed 8-bit
// values on the wire, so this is a reinterpretation, not a range conversion.
func signedByte(v int8) byte {
	return byte(v) //nolint:gosec // two's-complement reinterpretation is the wire format
}

// Character devices, in the order pkg/usbgadget links hid.GS0/GS1. Pointer
// report shapes, ID first:
//
//	relative: [1, buttons, dx, dy, wheel]   (5 bytes)
//	absolute: [2, buttons, x16, y16, wheel] (7 bytes)
const (
	keyboardDevice = "/dev/hidg0" // 8-byte boot keyboard: [mod, 0, k1..k6]
	mouseDevice    = "/dev/hidg1" // combined pointer function
)

// Pointer report IDs; must match pkg/usbgadget's descriptor.
const (
	reportIDRelMouse byte = 1
	reportIDAbsMouse byte = 2
)

const (
	// keyBufferSize is the boot keyboard's key-array length.
	keyBufferSize = 6
	// errorRollOver fills the array when more keys are held than it can carry,
	// which is what a real keyboard reports (HID 1.11 §B.1).
	errorRollOver = 0x01

	// writeTimeout bounds a report write. A hidg write blocks until the host
	// polls the endpoint, so with the host powered off or the cable out it
	// would block forever and take the caller's goroutine with it. A timeout
	// turns that into a dropped report, which is the correct outcome: there is
	// nobody to type at.
	writeTimeout = 250 * time.Millisecond
)

// autoReleaseAfter releases a non-modifier key that was never released
// explicitly. Browsers lose key-ups — window blur mid-keystroke, a modifier
// that swallows the up event, a closed tab — and a stuck key on the host is far
// worse than a slightly short one. Modifiers are exempt: they are meant to be
// held across other keys. A var so tests need not wait a second for it.
var autoReleaseAfter = 1 * time.Second

// LEDState is the keyboard's lock state, as reported *by the host* on the
// keyboard's OUT endpoint. It is authoritative — the host owns lock state, not
// the keyboard — which is why the virtual keyboard's indicators come from here
// rather than from what was typed.
type LEDState struct {
	NumLock    bool `json:"numLock"`
	CapsLock   bool `json:"capsLock"`
	ScrollLock bool `json:"scrollLock"`
	Compose    bool `json:"compose"`
	Kana       bool `json:"kana"`
}

// LED bit masks from HID 1.11 appendix B (the keyboard's output report).
const (
	ledNumLock    byte = 1 << 0
	ledCapsLock   byte = 1 << 1
	ledScrollLock byte = 1 << 2
	ledCompose    byte = 1 << 3
	ledKana       byte = 1 << 4
	ledValidMask       = ledNumLock | ledCapsLock | ledScrollLock | ledCompose | ledKana
)

func ledStateOf(b byte) LEDState {
	return LEDState{
		NumLock:    b&ledNumLock != 0,
		CapsLock:   b&ledCapsLock != 0,
		ScrollLock: b&ledScrollLock != 0,
		Compose:    b&ledCompose != 0,
		Kana:       b&ledKana != 0,
	}
}

// KeysDown is the keyboard report currently asserted: the modifier bits and the
// six-key array. Sent to clients so a reconnecting browser, or a second one,
// can show the same held keys the host sees.
type KeysDown struct {
	Modifier byte   `json:"modifier"`
	Keys     []byte `json:"keys"`
}

// Mouse button bits, shared by both mouse reports (HID usage page 0x09).
const (
	MouseLeft   byte = 1 << 0
	MouseRight  byte = 1 << 1
	MouseMiddle byte = 1 << 2
)

// AbsMax is the largest coordinate the absolute pointer's descriptor declares
// (LOGICAL_MAXIMUM 0x7fff). Clients scale the video rectangle onto 0..AbsMax.
const AbsMax = 0x7FFF

// Controller owns the HID character devices and the keyboard state machine.
// Safe for concurrent use: every keyboard mutation takes mu for the whole
// read-modify-write, so two callers (a browser key event and an auto-release
// timer, say) cannot interleave and lose an update.
type Controller struct {
	keyboard *device
	mouse    *device

	// mu guards keys and autoRelease, and serialises keyboard report writes.
	mu          sync.Mutex
	keys        KeysDown
	autoRelease map[byte]*time.Timer

	// ledMu guards leds and subs. Never taken while mu is held.
	ledMu sync.Mutex
	leds  byte
	subs  map[chan LEDState]struct{}

	// warnedAboveRange records that the descriptor-range warning has been
	// logged, so a held F13 does not repeat it every report.
	warnedAboveRange bool

	ledsOnce sync.Once
}

// NewController returns a Controller. No device is opened until first use: the
// gadget may not be bound yet at startup, and a BMC whose host never plugs in
// should not log open failures forever.
func NewController() *Controller {
	return &Controller{
		keyboard:    &device{path: keyboardDevice, flags: os.O_RDWR},
		mouse:       &device{path: mouseDevice, flags: os.O_WRONLY},
		keys:        KeysDown{Keys: make([]byte, keyBufferSize)},
		autoRelease: make(map[byte]*time.Timer),
		subs:        make(map[chan LEDState]struct{}),
	}
}

// ── Keyboard ──────────────────────────────────────────────────────────────

// Key presses or releases one key, updating and re-sending the whole report.
func (c *Controller) Key(code byte, press bool) error {
	if code == 0 {
		return nil
	}

	c.mu.Lock()
	modifier, keys := c.applyKey(code, press)
	err := c.writeKeyboard(modifier, keys)
	c.mu.Unlock()

	// Schedule outside the lock: the timer callback takes mu itself.
	if !IsModifier(code) {
		if press && keys[0] != errorRollOver {
			c.scheduleAutoRelease(code)
		} else {
			c.cancelAutoRelease(code)
		}
	}
	return err
}

// applyKey folds one key event into the held-key state and returns the report
// to send. Caller must hold mu.
func (c *Controller) applyKey(code byte, press bool) (byte, []byte) {
	modifier := c.keys.Modifier
	keys := append([]byte(nil), c.keys.Keys...)

	// A previous report rolled over; the host's view is unusable, so start
	// clean rather than compacting garbage.
	if keys[0] == errorRollOver {
		clear(keys)
	}

	if mask := ModifierMaskOf(code); mask != 0 {
		if press {
			modifier |= mask
		} else {
			modifier &^= mask
		}
		c.setKeys(modifier, keys)
		return modifier, keys
	}

	placed := false
	for i := range keyBufferSize {
		switch keys[i] {
		case code:
			// Already held: a press is a repeat (nothing to change), a
			// release removes it and closes the gap so the array stays dense.
			if !press {
				copy(keys[i:], keys[i+1:])
				keys[keyBufferSize-1] = 0
			}
			placed = true
		case 0:
			if press {
				keys[i] = code
			}
			placed = true
		}
		if placed {
			break
		}
	}

	if !placed && press {
		// More keys held than the array can carry. A real keyboard reports
		// rollover rather than silently dropping one.
		log.Warnf("hid: key buffer full, reporting rollover (key 0x%02x)", code)
		for i := range keys {
			keys[i] = errorRollOver
		}
	}

	c.setKeys(modifier, keys)
	return modifier, keys
}

// KeyReport asserts an explicit report, replacing whatever was held. This is
// the shape a macro step and a virtual-keyboard combo want: "these modifiers
// and these keys, now", with no history to reason about.
func (c *Controller) KeyReport(modifier byte, keys []byte) error {
	buf := make([]byte, keyBufferSize)
	copy(buf, keys)

	c.mu.Lock()
	defer c.mu.Unlock()

	// An explicit report supersedes every pending auto-release: those timers
	// exist to clean up keys nobody released, and this just released them.
	c.cancelAllAutoReleaseLocked()
	c.setKeys(modifier, buf)
	return c.writeKeyboard(modifier, buf)
}

// ReleaseAll clears the keyboard state. Called when a client disconnects or the
// video pane loses focus — otherwise a modifier held at that moment stays held
// on the host, which looks like broken hardware.
func (c *Controller) ReleaseAll() error {
	return c.KeyReport(0, nil)
}

// KeysDown returns the currently asserted report.
func (c *Controller) KeysDown() KeysDown {
	c.mu.Lock()
	defer c.mu.Unlock()
	return KeysDown{Modifier: c.keys.Modifier, Keys: append([]byte(nil), c.keys.Keys...)}
}

func (c *Controller) setKeys(modifier byte, keys []byte) {
	c.keys = KeysDown{Modifier: modifier, Keys: keys}
}

// writeKeyboard emits one 8-byte boot report. Caller must hold mu.
func (c *Controller) writeKeyboard(modifier byte, keys []byte) error {
	c.warnAboveRange(keys)

	report := make([]byte, 0, 2+keyBufferSize)
	report = append(report, modifier, 0x00)
	report = append(report, keys[:keyBufferSize]...)

	// Reading the host's LED reports needs the keyboard device open, and it
	// is only ever opened here, so arm the listener on first write.
	err := c.keyboard.write(report)
	if err == nil {
		c.ledsOnce.Do(func() { go c.watchLEDReports() })
	}
	return err
}

// warnAboveRange reports keys the gadget's descriptor does not declare, once.
// Without this they simply do nothing on the host and look like a bug here.
func (c *Controller) warnAboveRange(keys []byte) {
	if c.warnedAboveRange {
		return
	}
	for _, k := range keys {
		if k > MaxReportableKeyCode {
			c.warnedAboveRange = true
			log.Warnf("hid: key 0x%02x is above the keyboard descriptor's usage maximum (0x%02x); "+
				"the host will ignore it — widen the descriptor in pkg/usbgadget/hid.go to reach these keys", k, MaxReportableKeyCode)
			return
		}
	}
}

// ── Auto-release ──────────────────────────────────────────────────────────

func (c *Controller) scheduleAutoRelease(code byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t := c.autoRelease[code]; t != nil {
		t.Reset(autoReleaseAfter)
		return
	}
	c.autoRelease[code] = time.AfterFunc(autoReleaseAfter, func() { c.autoReleaseFire(code) })
}

func (c *Controller) cancelAutoRelease(code byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelAutoReleaseLocked(code)
}

func (c *Controller) cancelAutoReleaseLocked(code byte) {
	if t := c.autoRelease[code]; t != nil {
		t.Stop()
		delete(c.autoRelease, code)
	}
}

func (c *Controller) cancelAllAutoReleaseLocked() {
	for code, t := range c.autoRelease {
		if t != nil {
			t.Stop()
		}
		delete(c.autoRelease, code)
	}
}

// autoReleaseFire releases a key whose key-up never arrived.
func (c *Controller) autoReleaseFire(code byte) {
	c.mu.Lock()
	if _, pending := c.autoRelease[code]; !pending {
		c.mu.Unlock()
		return
	}
	delete(c.autoRelease, code)

	held := false
	for _, k := range c.keys.Keys {
		if k == code {
			held = true
			break
		}
	}
	if !held {
		c.mu.Unlock()
		return
	}

	log.Debugf("hid: auto-releasing key 0x%02x (no key-up received)", code)
	modifier, keys := c.applyKey(code, false)
	err := c.writeKeyboard(modifier, keys)
	c.mu.Unlock()

	if err != nil {
		log.Debugf("hid: auto-release of 0x%02x failed: %s", code, err)
	}
}

// ── Keyboard LED state ────────────────────────────────────────────────────

// LEDs returns the last lock state the host reported.
func (c *Controller) LEDs() LEDState {
	c.ledMu.Lock()
	defer c.ledMu.Unlock()
	return ledStateOf(c.leds)
}

// WatchLEDs returns a channel receiving the lock state on every change, plus a
// cancel func the caller must invoke. Latest-value semantics: a slow subscriber
// sees the current state, never a backlog.
func (c *Controller) WatchLEDs() (<-chan LEDState, func()) {
	ch := make(chan LEDState, 1)

	c.ledMu.Lock()
	c.subs[ch] = struct{}{}
	c.ledMu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.ledMu.Lock()
			delete(c.subs, ch)
			close(ch)
			c.ledMu.Unlock()
		})
	}
}

// watchLEDReports reads the keyboard's OUT reports for as long as the device
// stays open. The host writes one byte whenever a lock key changes — the
// boot keyboard has no Report IDs, so the report is the bare LED byte.
// Reading rather than inferring matters because lock state can change from a
// physically attached keyboard too.
func (c *Controller) watchLEDReports() {
	buf := make([]byte, 8)
	for {
		f := c.keyboard.handle()
		if f == nil {
			// Closed after a write error; it will be reopened by the next
			// write, which re-arms nothing — so stop and let that happen.
			return
		}

		n, err := f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			log.Debugf("hid: keyboard LED reader stopped: %s", err)
			return
		}
		if n < 1 {
			continue
		}
		c.updateLEDs(buf[0])
	}
}

func (c *Controller) updateLEDs(state byte) {
	if state&^ledValidMask != 0 {
		log.Debugf("hid: ignoring LED report with unknown bits (0x%02x)", state)
		return
	}

	c.ledMu.Lock()
	if c.leds == state {
		c.ledMu.Unlock()
		return
	}
	c.leds = state
	subs := make([]chan LEDState, 0, len(c.subs))
	for ch := range c.subs {
		subs = append(subs, ch)
	}
	c.ledMu.Unlock()

	led := ledStateOf(state)
	log.Debugf("hid: host LED state now %+v", led)
	for _, ch := range subs {
		// Non-blocking with replacement: the publisher must never stall on a
		// subscriber that has stopped reading.
		select {
		case ch <- led:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- led:
			default:
			}
		}
	}
}

// ── Mouse ─────────────────────────────────────────────────────────────────

// AbsMouse moves the pointer to an absolute position. x and y are in the
// descriptor's 0..AbsMax coordinate space, which the client maps from the video
// rectangle. This is the mode that keeps the host cursor under the browser
// cursor without pointer capture.
func (c *Controller) AbsMouse(x, y uint16, buttons byte, wheel int8) error {
	if x > AbsMax {
		x = AbsMax
	}
	if y > AbsMax {
		y = AbsMax
	}
	report := make([]byte, 7)
	report[0] = reportIDAbsMouse
	report[1] = buttons
	binary.LittleEndian.PutUint16(report[2:4], x)
	binary.LittleEndian.PutUint16(report[4:6], y)
	report[6] = signedByte(wheel)
	return c.mouse.write(report)
}

// RelMouse moves the pointer by a delta. Needed for hosts that ignore absolute
// pointers (some BIOS setup screens) and for pointer-locked play, where the
// browser reports deltas rather than positions.
func (c *Controller) RelMouse(dx, dy int8, buttons byte, wheel int8) error {
	return c.mouse.write([]byte{
		reportIDRelMouse, buttons, signedByte(dx), signedByte(dy), signedByte(wheel),
	})
}

// Close releases the character devices and stops pending auto-releases.
func (c *Controller) Close() {
	c.mu.Lock()
	c.cancelAllAutoReleaseLocked()
	c.mu.Unlock()

	c.keyboard.close()
	c.mouse.close()
}

// ── Character-device plumbing ─────────────────────────────────────────────

// device is one lazily-opened hidg character device. A write failure closes it
// so the next write reopens: the gadget is torn down and rebuilt whenever the
// host re-enumerates or the virtual-device config changes, which invalidates
// the fd underneath us.
type device struct {
	path  string
	flags int

	mu   sync.Mutex
	file *os.File

	// missing suppresses repeat open-failure logs for a gadget that is simply
	// not configured (no host attached, HID disabled).
	missing bool
}

func (d *device) handle() *os.File {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.file
}

func (d *device) open() (*os.File, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.file != nil {
		return d.file, nil
	}

	f, err := os.OpenFile(d.path, d.flags, 0)
	if err != nil {
		if !d.missing {
			d.missing = true
			log.Warnf("hid: %s unavailable: %s (is the USB gadget configured?)", d.path, err)
		}
		return nil, fmt.Errorf("open %s: %w", d.path, err)
	}
	d.missing = false
	d.file = f
	log.Debugf("hid: opened %s", d.path)
	return f, nil
}

func (d *device) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
}

// write emits one report, bounded by writeTimeout.
func (d *device) write(report []byte) error {
	f, err := d.open()
	if err != nil {
		return err
	}

	if err := f.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		// Not every kernel makes hidg pollable; without deadline support the
		// write still works, it just cannot be bounded.
		log.Debugf("hid: %s does not support write deadlines: %s", d.path, err)
	}

	if _, err := f.Write(report); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// Nobody is polling the endpoint — host off, cable out, or the
			// driver not yet bound. Dropping the report is right, and it is
			// not an error worth surfacing to the caller.
			log.Debugf("hid: %s write timed out; host is not polling", d.path)
			return nil
		}
		log.Debugf("hid: %s write failed: %s", d.path, err)
		d.close()
		return fmt.Errorf("write %s: %w", d.path, err)
	}
	return nil
}
