package redfish

// inventory.go owns the boot-override vocabulary and its rendering. The
// override itself is BMC state (hoststate.go): an operator stages it here
// and the host's firmware — a Redfish client on the USB host interface —
// reads it and applies it at boot. The BMC never reaches into host storage.

import (
	"github.com/stmcginnis/gofish/schemas"
)

// supportedBootSources are the BootSourceOverrideTarget values we accept on
// PATCH and advertise in AllowableValues.
var supportedBootSources = []schemas.BootSource{
	schemas.NoneBootSource,
	schemas.PxeBootSource,
	schemas.HddBootSource,
	schemas.CdBootSource,
	schemas.BiosSetupBootSource,
	schemas.UefiHTTPBootSource,
}

var supportedOverrideEnabled = []schemas.BootSourceOverrideEnabled{
	schemas.DisabledBootSourceOverrideEnabled,
	schemas.OnceBootSourceOverrideEnabled,
	schemas.ContinuousBootSourceOverrideEnabled,
}

// supportedResetTypes are the ComputerSystem.Reset values power.Controller
// can service.
var supportedResetTypes = []schemas.ResetType{
	schemas.OnResetType,
	schemas.ForceOffResetType,
	schemas.GracefulShutdownResetType,
	schemas.ForceRestartResetType,
	schemas.PowerCycleResetType,
}

// bootSourceSupported reports whether target is one we accept.
func bootSourceSupported(target schemas.BootSource) bool {
	for _, s := range supportedBootSources {
		if s == target {
			return true
		}
	}
	return false
}

// overrideEnabledSupported reports whether e is one we accept.
func overrideEnabledSupported(e schemas.BootSourceOverrideEnabled) bool {
	for _, s := range supportedOverrideEnabled {
		if s == e {
			return true
		}
	}
	return false
}

// resetTypeSupported reports whether t is one we can service.
func resetTypeSupported(t schemas.ResetType) bool {
	for _, s := range supportedResetTypes {
		if s == t {
			return true
		}
	}
	return false
}

// readBoot renders the staged override as the ComputerSystem Boot block.
func readBoot() Boot {
	override := BootOverride()
	bootOptionsLink := Link(bootOptionsPath)
	boot := Boot{
		BootSourceOverrideTarget:  schemas.NoneBootSource,
		BootSourceOverrideEnabled: schemas.DisabledBootSourceOverrideEnabled,
		// The managed host's firmware path is UEFI-only, so there is no
		// Legacy toggle to honour. The property is still reported so PATCH
		// echoes match.
		BootSourceOverrideMode: schemas.UEFIBootSourceOverrideMode,
		BootOptions:            &bootOptionsLink,
		AllowableTargets:       supportedBootSources,
		AllowableEnabled:       supportedOverrideEnabled,
		AllowableModes:         []schemas.BootSourceOverrideMode{schemas.UEFIBootSourceOverrideMode},
	}
	if override.Target != "" {
		boot.BootSourceOverrideTarget = schemas.BootSource(override.Target)
	}
	if override.Enabled != "" {
		boot.BootSourceOverrideEnabled = schemas.BootSourceOverrideEnabled(override.Enabled)
	}
	return boot
}

// stageBootOverride stages an override for the host firmware to consume.
func stageBootOverride(target schemas.BootSource, enabled schemas.BootSourceOverrideEnabled) {
	SetBootOverride(string(target), string(enabled))
}

// clearBootOverride removes any staged override.
func clearBootOverride() {
	SetBootOverride(string(schemas.NoneBootSource),
		string(schemas.DisabledBootSourceOverrideEnabled))
}
