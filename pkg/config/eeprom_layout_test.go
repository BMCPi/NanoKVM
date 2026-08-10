package config

// eeprom_layout_test.go pins the three EEPROM regions apart. efivars,
// ubootenv and smbios all address one 24c256 through the same backing file, so
// a region that overruns its neighbour silently corrupts it — and for the env,
// whose size is also its CRC length, a wrong size makes U-Boot reject an
// intact environment with "bad CRC, using default environment".

import "testing"

const eepromPath = "/sys/bus/i2c/devices/0-1050/slave-eeprom"

// withInstance runs fn against a fresh config instance, restoring the shared
// package state afterwards.
func withInstance(t *testing.T, c Config, fn func()) {
	t.Helper()
	saved := instance
	t.Cleanup(func() { instance = saved })
	instance = c
	fn()
}

// baseConfig returns the shipped defaults for the three stores.
func baseConfig() Config {
	return Config{
		EfiVars:  defaultConfig.EfiVars,
		UbootEnv: defaultConfig.UbootEnv,
		SMBIOS:   defaultConfig.SMBIOS,
	}
}

// The shipped defaults must already tile the chip without overlap.
func TestDefaultEEPROMRegionsDoNotOverlap(t *testing.T) {
	withInstance(t, baseConfig(), func() {
		checkDefaultValue()

		uefiEnd := instance.EfiVars.StoreSize
		envStart := instance.UbootEnv.Offset
		envEnd := envStart + instance.UbootEnv.Size
		smbiosStart := instance.SMBIOS.Offset
		smbiosEnd := smbiosStart + instance.SMBIOS.Size

		if uefiEnd > envStart {
			t.Errorf("UEFI region ends at %#x, past env start %#x", uefiEnd, envStart)
		}
		if envEnd > smbiosStart {
			t.Errorf("env region ends at %#x, past SMBIOS start %#x", envEnd, smbiosStart)
		}
		if smbiosEnd > 32768 {
			t.Errorf("SMBIOS region ends at %#x, past the 32K chip", smbiosEnd)
		}

		// The host's layout, which these must mirror exactly.
		for _, tc := range []struct {
			name      string
			got, want int
		}{
			{"env offset (CONFIG_ENV_OFFSET)", envStart, 0x4000},
			{"env size (CONFIG_ENV_SIZE)", instance.UbootEnv.Size, 0x2000},
			{"smbios offset", smbiosStart, 0x6000},
			{"smbios size", instance.SMBIOS.Size, 0x800},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %#x, want %#x", tc.name, tc.got, tc.want)
			}
		}
	})
}

// A server.yaml written before CONFIG_ENV_SIZE shrank holds size: 16384. It is
// non-zero, so the `<= 0` backfill leaves it alone — it must be clamped, or the
// BMC CRCs 16380 bytes while U-Boot CRCs 8188 and every read reports bad CRC.
func TestLegacyEnvSizeIsClamped(t *testing.T) {
	c := baseConfig()
	c.UbootEnv.Size = 0x4000 // the old CONFIG_ENV_SIZE

	withInstance(t, c, func() {
		checkDefaultValue()

		if instance.UbootEnv.Size != 0x2000 {
			t.Errorf("env size = %#x, want it clamped to %#x",
				instance.UbootEnv.Size, 0x2000)
		}
		if end := instance.UbootEnv.Offset + instance.UbootEnv.Size; end > instance.SMBIOS.Offset {
			t.Errorf("env still overruns SMBIOS: ends %#x, SMBIOS starts %#x",
				end, instance.SMBIOS.Offset)
		}
	})
}

// The mirror case: a config holding the old whole-chip storeSize.
func TestLegacyEfiVarsStoreSizeIsClamped(t *testing.T) {
	c := baseConfig()
	c.EfiVars.StoreSize = 32768 // the old whole-chip value

	withInstance(t, c, func() {
		checkDefaultValue()

		if instance.EfiVars.StoreSize != instance.UbootEnv.Offset {
			t.Errorf("storeSize = %#x, want it clamped to the env offset %#x",
				instance.EfiVars.StoreSize, instance.UbootEnv.Offset)
		}
	})
}

// Clamping is only sound because the regions share a device. A store pointed at
// its own file must keep the size the operator asked for.
func TestClampsOnlyApplyWhenRegionsShareADevice(t *testing.T) {
	c := baseConfig()
	c.UbootEnv.Size = 0x4000
	c.EfiVars.StoreSize = 32768
	// Separate devices — no region can collide.
	c.EfiVars.Path = "/tmp/uefi.bin"
	c.UbootEnv.Path = "/tmp/env.bin"
	c.SMBIOS.Path = "/tmp/smbios.bin"

	withInstance(t, c, func() {
		checkDefaultValue()

		if instance.UbootEnv.Size != 0x4000 {
			t.Errorf("env size = %#x on a private device, want it left at %#x",
				instance.UbootEnv.Size, 0x4000)
		}
		if instance.EfiVars.StoreSize != 32768 {
			t.Errorf("storeSize = %#x on a private device, want it left at 32768",
				instance.EfiVars.StoreSize)
		}
	})
}

// A disabled SMBIOS store frees the space above the env, so nothing is clamped.
func TestEnvNotClampedWhenSMBIOSDisabled(t *testing.T) {
	c := baseConfig()
	c.UbootEnv.Size = 0x4000
	c.SMBIOS.Enabled = false

	withInstance(t, c, func() {
		checkDefaultValue()

		if instance.UbootEnv.Size != 0x4000 {
			t.Errorf("env size = %#x with SMBIOS disabled, want %#x",
				instance.UbootEnv.Size, 0x4000)
		}
	})
}

// An unset size must still land on the shipped default rather than the clamp.
func TestZeroEnvSizeGetsDefault(t *testing.T) {
	c := baseConfig()
	c.UbootEnv.Size = 0
	c.UbootEnv.Offset = 0

	withInstance(t, c, func() {
		checkDefaultValue()

		if instance.UbootEnv.Offset != 0x4000 {
			t.Errorf("env offset = %#x, want the default %#x", instance.UbootEnv.Offset, 0x4000)
		}
		if instance.UbootEnv.Size != 0x2000 {
			t.Errorf("env size = %#x, want the default %#x", instance.UbootEnv.Size, 0x2000)
		}
	})
}
