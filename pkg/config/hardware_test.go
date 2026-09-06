package config

import (
	"os"
	"strings"
	"testing"

	"github.com/warthog618/go-gpiosim"
)

// newSim brings up a simulated gpiochip, skipping the test where gpio-sim is
// unavailable (it needs root and the gpio_sim module), as the other GPIO tests
// in this tree do.
func newSim(t *testing.T, bank *gpiosim.Bank) *gpiosim.Sim {
	t.Helper()

	sim, err := gpiosim.NewSim(gpiosim.WithBank(bank))
	if err != nil {
		t.Skipf("gpio-sim unavailable (needs root + gpio-sim module): %v", err)
	}
	t.Cleanup(sim.Close)
	return sim
}

// TestResolveLinesByName is the regression test for the failure this resolution
// scheme exists to prevent: /dev/gpiochipN numbering is assigned in controller
// registration order, so a hardcoded chip and offset silently addresses the
// wrong pad once any GPIO node is added to the device tree. The names here sit
// at offsets deliberately unlike the profile's, and on a chip whose label does
// not match the fallback, so only a name lookup can produce them.
func TestResolveLinesByName(t *testing.T) {
	const (
		powerOffset    = 3
		powerLEDOffset = 9
		hddLEDOffset   = 12
		resetOffset    = 20
	)

	sim := newSim(t, gpiosim.NewBank("sim-not-the-header-bank", 32,
		gpiosim.WithNamedLine(powerOffset, lineNamePower),
		gpiosim.WithNamedLine(powerLEDOffset, lineNamePowerLED),
		gpiosim.WithNamedLine(hddLEDOffset, lineNameHDDLed),
		gpiosim.WithNamedLine(resetOffset, lineNameReset),
	))
	chip := sim.Chips[0].ChipName()

	hw := profileAlpha.hardware()

	for _, tc := range []struct {
		role string
		got  GPIOPin
		want GPIOPin
	}{
		{"power", hw.GPIOPower, GPIOPin{Chip: chip, Line: powerOffset}},
		{"power-LED", hw.GPIOPowerLED, GPIOPin{Chip: chip, Line: powerLEDOffset}},
		{"HDD-LED", hw.GPIOHDDLed, GPIOPin{Chip: chip, Line: hddLEDOffset}},
		{"reset", hw.GPIOReset, GPIOPin{Chip: chip, Line: resetOffset}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s pin = %s, want %s", tc.role, tc.got, tc.want)
		}
	}
}

// TestResolveLinesByChipLabel covers a device tree that names no lines: the bank
// is then found by its label, which is its MMIO address and so does not move
// with enumeration order either, and the profile supplies the offsets.
func TestResolveLinesByChipLabel(t *testing.T) {
	sim := newSim(t, gpiosim.NewBank(headerChipLabel, 32))
	chip := sim.Chips[0].ChipName()

	hw := profileBeta.hardware()

	if want := (GPIOPin{Chip: chip, Line: profileBeta.power}); hw.GPIOPower != want {
		t.Errorf("power pin = %s, want %s", hw.GPIOPower, want)
	}
	if want := (GPIOPin{Chip: chip, Line: profileBeta.powerLED}); hw.GPIOPowerLED != want {
		t.Errorf("power-LED pin = %s, want %s", hw.GPIOPowerLED, want)
	}
	// Beta has no HDD LED, and no name lookup should invent one for it.
	if !hw.GPIOHDDLed.IsZero() {
		t.Errorf("HDD-LED pin = %s, want unset", hw.GPIOHDDLed)
	}
}

// TestResolveUnknownLineIsUnset checks the deliberate dead end: a line that
// neither a name nor the fallback label can locate must come back unset rather
// than pointing at a guess, because driving the wrong pin fails silently.
func TestResolveUnknownLineIsUnset(t *testing.T) {
	newSim(t, gpiosim.NewBank("sim-unrelated-bank", 8))

	var r lineResolver
	if pin := r.resolve("no-such-line-name", 23); !pin.IsZero() {
		t.Errorf("pin = %s, want unset", pin)
	}
}

func TestFindChipByLabelRejectsUnknown(t *testing.T) {
	if _, err := findChipByLabel("definitely-not-a-real-chip-label"); err == nil {
		t.Error("expected an error for an unmatched label")
	}
}

// TestFanControlDefaultsFromProfile covers the common case: a config that
// omits hardware.fanControl comes up with the running profile's default
// (true on every profile today, see hwProfile.fanControl), not a false
// zero value that would silently hide the Chassis Oem.PiBmc.FanOverrideLevel
// knob.
func TestFanControlDefaultsFromProfile(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret)

	want := getHardware().FanControl
	if want == nil {
		t.Fatal("getHardware() returned a nil FanControl default")
	}
	if c.Hardware.FanControl == nil || *c.Hardware.FanControl != *want {
		t.Errorf("Hardware.FanControl = %v, want the profile default %v", c.Hardware.FanControl, *want)
	}
}

// TestFanControlExplicitOverridePassesThrough covers an operator explicitly
// disabling fan control on a profile that defaults it on: a plain zero-value
// check cannot tell that apart from an absent key, so applyHardwareDefaults'
// wholesale rebuild via getHardware() must not clobber it.
func TestFanControlExplicitOverridePassesThrough(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret+"hardware:\n  fanControl: false\n")

	if c.Hardware.FanControl == nil || *c.Hardware.FanControl {
		t.Errorf("Hardware.FanControl = %v, want explicit false to survive defaulting", c.Hardware.FanControl)
	}
}

// TestFanControlExplicitTrueOverridePassesThrough is the other explicit
// value, on the chance a future profile ever defaults false.
func TestFanControlExplicitTrueOverridePassesThrough(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret+"hardware:\n  fanControl: true\n")

	if c.Hardware.FanControl == nil || !*c.Hardware.FanControl {
		t.Errorf("Hardware.FanControl = %v, want explicit true to survive defaulting", c.Hardware.FanControl)
	}
}

// TestFanControlFalseOverrideSurvivesPersist is the regression test for the
// bool-zero-value trap on the write side: an operator's explicit
// "fanControl: false" must not be indistinguishable from "unset" the next
// time this package rewrites the file (e.g. an unrelated settings.Save()).
// A plain bool with `omitempty` would drop the key when false, silently
// reverting to the profile default (true) on the next boot; FanControl is a
// *bool precisely so a non-nil pointer to false still marshals explicitly.
func TestFanControlFalseOverrideSurvivesPersist(t *testing.T) {
	_, path := loadConfigFromYAML(t, migrateSecret+"hardware:\n  fanControl: false\n")

	persistConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if !strings.Contains(string(data), "fanControl: false") {
		t.Errorf("persisted config lost the explicit fanControl: false override:\n%s", data)
	}
}
