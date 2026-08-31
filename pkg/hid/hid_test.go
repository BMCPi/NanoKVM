package hid

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestController points the three devices at ordinary files, which accept
// writes exactly as the character devices do, so every report the controller
// emits can be read back and asserted byte for byte.
func newTestController(t *testing.T) *Controller {
	t.Helper()

	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	// The real devices already exist as character devices, so the opener passes
	// no create mode; create the stand-ins here with a readable one.
	stub := func(name string) *device {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		return &device{path: path, flags: os.O_RDWR, log: log}
	}

	c := NewController(log)
	c.keyboard = stub("hidg0")
	c.mouse = stub("hidg1")
	t.Cleanup(c.Close)
	return c
}

// idReports splits the combined pointer device's bytes into fixed-size
// reports of one collection, verifying and stripping each report's leading
// Report ID so the assertions below stay in payload terms.
func idReports(t *testing.T, d *device, id byte, size int) [][]byte {
	t.Helper()
	raw := reports(t, d, size+1)
	out := make([][]byte, 0, len(raw))
	for i, r := range raw {
		if r[0] != id {
			t.Fatalf("report %d has ID %#x, want %#x", i, r[0], id)
		}
		out = append(out, r[1:])
	}
	return out
}

// reports splits a device's accumulated bytes into fixed-size reports.
func reports(t *testing.T, d *device, size int) [][]byte {
	t.Helper()

	data, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatalf("read %s: %v", d.path, err)
	}
	if len(data)%size != 0 {
		t.Fatalf("%s holds %d bytes, not a whole number of %d-byte reports", d.path, len(data), size)
	}

	var out [][]byte
	for i := 0; i < len(data); i += size {
		out = append(out, data[i:i+size])
	}
	return out
}

func wantReport(t *testing.T, got []byte, want ...byte) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("report length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("report = % x, want % x", got, want)
		}
	}
}

// TestKeyboardReportsHeldState is the property the boot protocol demands: each
// report states every key currently down, not the one that just changed. A host
// applying these reports in order must see A, then shift+A, then shift alone.
func TestKeyboardReportsHeldState(t *testing.T) {
	c := newTestController(t)

	for _, step := range []struct {
		code  byte
		press bool
	}{
		{KeyCodes["KeyA"], true},
		{KeyLeftShift, true},
		{KeyCodes["KeyA"], false},
	} {
		if err := c.Key(step.code, step.press); err != nil {
			t.Fatalf("key 0x%02x press=%t: %v", step.code, step.press, err)
		}
	}

	got := reports(t, c.keyboard, 8)
	if len(got) != 3 {
		t.Fatalf("got %d reports, want 3", len(got))
	}
	wantReport(t, got[0], 0x00, 0x00, 0x04, 0, 0, 0, 0, 0)
	wantReport(t, got[1], ModLeftShift, 0x00, 0x04, 0, 0, 0, 0, 0)
	wantReport(t, got[2], ModLeftShift, 0x00, 0x00, 0, 0, 0, 0, 0)
}

// TestKeyBufferCompaction covers the release path: removing a key from the
// middle of the array must close the gap. A host reading a sparse array treats
// the zero as the end of the list and ignores everything after it, so a stale
// key would appear stuck.
func TestKeyBufferCompaction(t *testing.T) {
	c := newTestController(t)

	for _, name := range []string{"KeyA", "KeyB", "KeyC"} {
		if err := c.Key(KeyCodes[name], true); err != nil {
			t.Fatalf("press %s: %v", name, err)
		}
	}
	if err := c.Key(KeyCodes["KeyB"], false); err != nil {
		t.Fatalf("release KeyB: %v", err)
	}

	all := reports(t, c.keyboard, 8)
	wantReport(t, all[len(all)-1], 0x00, 0x00, 0x04, 0x06, 0, 0, 0, 0)

	if down := c.KeysDown(); down.Keys[0] != 0x04 || down.Keys[1] != 0x06 || down.Keys[2] != 0 {
		t.Errorf("keys down = % x, want 04 06 then zeros", down.Keys)
	}
}

// TestKeyBufferRollover checks the overflow report. A seventh simultaneous key
// does not fit the boot protocol's array, and a real keyboard says so by filling
// every slot with ErrorRollOver rather than dropping one silently.
func TestKeyBufferRollover(t *testing.T) {
	c := newTestController(t)

	for _, name := range []string{"KeyA", "KeyB", "KeyC", "KeyD", "KeyE", "KeyF", "KeyG"} {
		if err := c.Key(KeyCodes[name], true); err != nil {
			t.Fatalf("press %s: %v", name, err)
		}
	}

	all := reports(t, c.keyboard, 8)
	wantReport(t, all[len(all)-1], 0x00, 0x00,
		errorRollOver, errorRollOver, errorRollOver, errorRollOver, errorRollOver, errorRollOver)
}

// TestModifiersAreNotInTheKeyArray pins the split the descriptor defines:
// modifier keycodes set bits in byte 0 and must never consume a key slot, or six
// held modifiers would exhaust the array.
func TestModifiersAreNotInTheKeyArray(t *testing.T) {
	c := newTestController(t)

	for _, code := range []byte{KeyLeftControl, KeyLeftAlt, KeyRightShift, KeyLeftSuper} {
		if err := c.Key(code, true); err != nil {
			t.Fatalf("press modifier 0x%02x: %v", code, err)
		}
	}

	down := c.KeysDown()
	wantMod := ModLeftControl | ModLeftAlt | ModRightShift | ModLeftSuper
	if down.Modifier != wantMod {
		t.Errorf("modifier = %08b, want %08b", down.Modifier, wantMod)
	}
	for i, k := range down.Keys {
		if k != 0 {
			t.Errorf("key slot %d = 0x%02x, want empty", i, k)
		}
	}
}

// TestReleaseAllClearsState is what runs when a client disconnects or the pane
// loses focus: whatever was held must be let go, or the host keeps repeating it.
func TestReleaseAllClearsState(t *testing.T) {
	c := newTestController(t)

	if err := c.Key(KeyLeftControl, true); err != nil {
		t.Fatalf("press ctrl: %v", err)
	}
	if err := c.Key(KeyCodes["KeyC"], true); err != nil {
		t.Fatalf("press c: %v", err)
	}
	if err := c.ReleaseAll(); err != nil {
		t.Fatalf("release all: %v", err)
	}

	all := reports(t, c.keyboard, 8)
	wantReport(t, all[len(all)-1], 0, 0, 0, 0, 0, 0, 0, 0)

	if down := c.KeysDown(); down.Modifier != 0 {
		t.Errorf("modifier = %08b after ReleaseAll, want 0", down.Modifier)
	}
}

// TestAutoReleaseFiresForLostKeyUp covers the safety net. A browser that loses a
// key-up (blur mid-keystroke) would otherwise leave the key held on the host
// forever.
func TestAutoReleaseFiresForLostKeyUp(t *testing.T) {
	saved := autoReleaseAfter
	autoReleaseAfter = 30 * time.Millisecond
	t.Cleanup(func() { autoReleaseAfter = saved })

	c := newTestController(t)
	if err := c.Key(KeyCodes["KeyZ"], true); err != nil {
		t.Fatalf("press: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for c.KeysDown().Keys[0] != 0 {
		if time.Now().After(deadline) {
			t.Fatal("key was never auto-released")
		}
		time.Sleep(5 * time.Millisecond)
	}

	all := reports(t, c.keyboard, 8)
	wantReport(t, all[len(all)-1], 0, 0, 0, 0, 0, 0, 0, 0)
}

// TestAutoReleaseCancelledByExplicitKeyUp makes sure the safety net does not
// fire a second, spurious release after a normal key-up.
func TestAutoReleaseCancelledByExplicitKeyUp(t *testing.T) {
	saved := autoReleaseAfter
	autoReleaseAfter = 30 * time.Millisecond
	t.Cleanup(func() { autoReleaseAfter = saved })

	c := newTestController(t)
	if err := c.Key(KeyCodes["KeyZ"], true); err != nil {
		t.Fatalf("press: %v", err)
	}
	if err := c.Key(KeyCodes["KeyZ"], false); err != nil {
		t.Fatalf("release: %v", err)
	}

	before := len(reports(t, c.keyboard, 8))
	time.Sleep(120 * time.Millisecond)
	if after := len(reports(t, c.keyboard, 8)); after != before {
		t.Errorf("%d extra report(s) written after an explicit key-up", after-before)
	}
}

// TestKeyReportReplacesState is the macro/combo path: an explicit report says
// what is held, with no dependence on what came before.
func TestKeyReportReplacesState(t *testing.T) {
	c := newTestController(t)

	if err := c.Key(KeyCodes["KeyA"], true); err != nil {
		t.Fatalf("press a: %v", err)
	}
	if err := c.KeyReport(ModLeftControl|ModLeftAlt, []byte{KeyCodes["Delete"]}); err != nil {
		t.Fatalf("key report: %v", err)
	}

	all := reports(t, c.keyboard, 8)
	wantReport(t, all[len(all)-1], ModLeftControl|ModLeftAlt, 0x00, 0x4c, 0, 0, 0, 0, 0)
}

// TestAbsMouseReportLayout pins the 6-byte absolute report the descriptor in
// pkg/usbgadget declares: buttons, then little-endian 16-bit X and Y, then a
// signed wheel. Getting the byte order wrong sends the pointer to a corner.
func TestAbsMouseReportLayout(t *testing.T) {
	c := newTestController(t)

	if err := c.AbsMouse(0x1234, 0x7FFF, MouseLeft, -3); err != nil {
		t.Fatalf("abs mouse: %v", err)
	}

	got := idReports(t, c.mouse, reportIDAbsMouse, 6)
	wantReport(t, got[0], MouseLeft, 0x34, 0x12, 0xFF, 0x7F, 0xFD)
}

// TestAbsMouseClampsToDescriptorRange keeps coordinates inside the declared
// logical maximum; a host given more than it was promised may discard the report.
func TestAbsMouseClampsToDescriptorRange(t *testing.T) {
	c := newTestController(t)

	if err := c.AbsMouse(0xFFFF, 0xFFFF, 0, 0); err != nil {
		t.Fatalf("abs mouse: %v", err)
	}

	got := idReports(t, c.mouse, reportIDAbsMouse, 6)
	wantReport(t, got[0], 0, 0xFF, 0x7F, 0xFF, 0x7F, 0)
}

// TestRelMouseReportLayout pins the 4-byte relative report, including negative
// deltas as two's complement.
func TestRelMouseReportLayout(t *testing.T) {
	c := newTestController(t)

	if err := c.RelMouse(-1, 5, MouseRight|MouseMiddle, 1); err != nil {
		t.Fatalf("rel mouse: %v", err)
	}

	got := idReports(t, c.mouse, reportIDRelMouse, 4)
	wantReport(t, got[0], MouseRight|MouseMiddle, 0xFF, 0x05, 0x01)
}

// TestLEDStateFromHostReport covers the lock state the host pushes back on the
// keyboard's OUT endpoint, which is where the indicators come from.
func TestLEDStateFromHostReport(t *testing.T) {
	c := newTestController(t)

	ch, cancel := c.WatchLEDs()
	defer cancel()

	c.updateLEDs(ledCapsLock | ledNumLock)

	select {
	case got := <-ch:
		if !got.CapsLock || !got.NumLock || got.ScrollLock {
			t.Errorf("led state = %+v, want caps+num only", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no LED state delivered to subscriber")
	}

	if got := c.LEDs(); !got.CapsLock || !got.NumLock {
		t.Errorf("LEDs() = %+v, want caps+num", got)
	}
}

// TestLEDStateIgnoresUnknownBits guards against a host report with bits outside
// the descriptor's five LEDs being decoded as lock state.
func TestLEDStateIgnoresUnknownBits(t *testing.T) {
	c := newTestController(t)

	c.updateLEDs(0x80)
	if got := c.LEDs(); got.NumLock || got.CapsLock || got.ScrollLock || got.Compose || got.Kana {
		t.Errorf("led state = %+v, want all clear", got)
	}
}

// TestKeyCodeTable spot-checks the table ported from JetKVM. These are the
// codes a browser's KeyboardEvent.code maps to, and a wrong entry types the
// wrong character on the host.
func TestKeyCodeTable(t *testing.T) {
	for name, want := range map[string]byte{
		"KeyA":         0x04,
		"KeyZ":         0x1d,
		"Digit1":       0x1e,
		"Enter":        0x28,
		"Escape":       0x29,
		"Space":        0x2c,
		"Delete":       0x4c,
		"ArrowRight":   0x4f,
		"F1":           0x3a,
		"F12":          0x45,
		"ControlLeft":  0xe0,
		"ShiftLeft":    0xe1,
		"AltLeft":      0xe2,
		"MetaLeft":     0xe3,
		"ControlRight": 0xe4,
	} {
		got, ok := CodeFor(name)
		if !ok {
			t.Errorf("%s missing from KeyCodes", name)
			continue
		}
		if got != want {
			t.Errorf("%s = 0x%02x, want 0x%02x", name, got, want)
		}
	}
}

// TestModifierClassification pins which codes are modifiers, since the report
// layout depends entirely on that split.
func TestModifierClassification(t *testing.T) {
	for _, code := range []byte{
		KeyLeftControl, KeyLeftShift, KeyLeftAlt, KeyLeftSuper,
		KeyRightControl, KeyRightShift, KeyRightAlt, KeyRightSuper,
	} {
		if !IsModifier(code) {
			t.Errorf("0x%02x should be a modifier", code)
		}
		if ModifierMaskOf(code) == 0 {
			t.Errorf("0x%02x has no modifier mask", code)
		}
	}

	for _, name := range []string{"KeyA", "Enter", "F1", "CapsLock"} {
		if code := KeyCodes[name]; IsModifier(code) {
			t.Errorf("%s (0x%02x) should not be a modifier", name, code)
		}
	}
}
