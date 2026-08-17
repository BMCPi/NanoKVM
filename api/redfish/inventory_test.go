package redfish

import (
	"testing"

	"github.com/stmcginnis/gofish/schemas"
)

func TestResetTypeSupported(t *testing.T) {
	for _, ok := range supportedResetTypes {
		if !resetTypeSupported(ok) {
			t.Errorf("%q should be supported", ok)
		}
	}
	// Types gofish defines but power.Controller cannot service.
	for _, bad := range []schemas.ResetType{
		schemas.NmiResetType,
		schemas.PushPowerButtonResetType,
		schemas.GracefulRestartResetType,
		"", "Bogus",
	} {
		if resetTypeSupported(bad) {
			t.Errorf("%q should not be supported", bad)
		}
	}
}

func TestBootSourceSupported(t *testing.T) {
	if !bootSourceSupported(schemas.PxeBootSource) {
		t.Error("Pxe should be supported")
	}
	for _, bad := range []schemas.BootSource{
		schemas.FloppyBootSource,
		schemas.UefiShellBootSource,
		schemas.SDCardBootSource,
		"", "Bogus",
	} {
		if bootSourceSupported(bad) {
			t.Errorf("%q should not be supported", bad)
		}
	}
}

// The staged override is plain BMC state; readBoot must render exactly what
// applyBootPatch staged, because the host firmware reads the override out of
// this rendering.
func TestApplyBootPatchRoundTrips(t *testing.T) {
	t.Cleanup(clearBootOverride)

	for _, tc := range []struct {
		name        string
		target      schemas.BootSource
		enabled     schemas.BootSourceOverrideEnabled
		wantTarget  schemas.BootSource
		wantEnabled schemas.BootSourceOverrideEnabled
	}{
		{
			"once is the patch default", schemas.PxeBootSource, "",
			schemas.PxeBootSource, schemas.OnceBootSourceOverrideEnabled,
		},
		{
			"continuous sticks", schemas.HddBootSource, schemas.ContinuousBootSourceOverrideEnabled,
			schemas.HddBootSource, schemas.ContinuousBootSourceOverrideEnabled,
		},
		{
			"disabled clears", schemas.PxeBootSource, schemas.DisabledBootSourceOverrideEnabled,
			schemas.NoneBootSource, schemas.DisabledBootSourceOverrideEnabled,
		},
		{
			"none clears", schemas.NoneBootSource, schemas.OnceBootSourceOverrideEnabled,
			schemas.NoneBootSource, schemas.DisabledBootSourceOverrideEnabled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyBootPatch(tc.target, tc.enabled); err != nil {
				t.Fatalf("applyBootPatch: %v", err)
			}
			boot := readBoot()
			if boot.BootSourceOverrideTarget != tc.wantTarget {
				t.Errorf("target = %q, want %q", boot.BootSourceOverrideTarget, tc.wantTarget)
			}
			if boot.BootSourceOverrideEnabled != tc.wantEnabled {
				t.Errorf("enabled = %q, want %q", boot.BootSourceOverrideEnabled, tc.wantEnabled)
			}
		})
	}
}

func TestApplyBootPatchRejectsInvalidValues(t *testing.T) {
	t.Cleanup(clearBootOverride)

	for _, tc := range []struct {
		name    string
		target  schemas.BootSource
		enabled schemas.BootSourceOverrideEnabled
	}{
		{"unknown target", schemas.FloppyBootSource, schemas.OnceBootSourceOverrideEnabled},
		{"unknown enabled", schemas.PxeBootSource, "Sometimes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyBootPatch(tc.target, tc.enabled); err == nil {
				t.Errorf("applyBootPatch(%q, %q) accepted an invalid value", tc.target, tc.enabled)
			}
		})
	}
	// A rejected patch must not have disturbed the (cleared) override.
	if boot := readBoot(); boot.BootSourceOverrideTarget != schemas.NoneBootSource {
		t.Errorf("override changed by a rejected patch: %+v", boot)
	}
}

// The Boot block must always advertise the BootOptions collection the host
// populates — that link is how clients give BootOrder references meaning.
func TestReadBootLinksBootOptions(t *testing.T) {
	boot := readBoot()
	if boot.BootOptions == nil || boot.BootOptions.String() != bootOptionsPath {
		t.Errorf("Boot.BootOptions = %v, want %s", boot.BootOptions, bootOptionsPath)
	}
	if len(boot.AllowableTargets) != len(supportedBootSources) {
		t.Errorf("AllowableTargets has %d entries, want %d",
			len(boot.AllowableTargets), len(supportedBootSources))
	}
}
