package config

// power_test.go covers power.reset defaulting and validation: an absent key
// must default to "auto", an explicit valid value must pass through
// untouched, and anything else must be rejected at load rather than
// silently coerced (see applyPowerDefaults).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// TestPowerResetDefaultsToAuto covers the common case: a config file that
// predates power.reset (or simply omits it) must come up in "auto" dispatch,
// not an empty string that every consumer would have to special-case.
func TestPowerResetDefaultsToAuto(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret)

	if c.Power.Reset != PowerResetAuto {
		t.Errorf("Power.Reset = %q, want %q", c.Power.Reset, PowerResetAuto)
	}
}

// TestPowerResetExplicitValuesPassThrough covers the two non-default
// sentinels: an operator's explicit choice must survive defaulting
// unchanged.
func TestPowerResetExplicitValuesPassThrough(t *testing.T) {
	for _, want := range []string{PowerResetLine, PowerResetCycle} {
		t.Run(want, func(t *testing.T) {
			c, _ := loadConfigFromYAML(t, migrateSecret+"power:\n  reset: "+want+"\n")

			if c.Power.Reset != want {
				t.Errorf("Power.Reset = %q, want %q", c.Power.Reset, want)
			}
		})
	}
}

// TestPowerResetInvalidValueRejected is the config test for the reject half
// of the brief: a mistyped power.reset must fail the load with a clear
// error naming the bad value, not silently coerce to a policy the operator
// never asked for. This can't use loadConfigFromYAML — that helper
// t.Fatals on exactly the error this test expects — so it inlines the same
// viper.ReadConfig + Unmarshal + checkDefaultValue sequence to observe the
// error directly.
func TestPowerResetInvalidValueRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	origPath := configFilePath
	configFilePath = path
	t.Cleanup(func() { configFilePath = origPath })

	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.SetConfigType("yaml")
	yamlText := migrateSecret + "power:\n  reset: bogus\n"
	if err := viper.ReadConfig(bytes.NewBufferString(yamlText)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	instance = Config{}
	if err := viper.Unmarshal(&instance); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	err := checkDefaultValue()
	if err == nil {
		t.Fatal("checkDefaultValue = nil error, want power.reset: \"bogus\" to be rejected")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error = %q, want it to name the rejected value", err)
	}

	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("a rejected config was still persisted to disk")
	}
}
