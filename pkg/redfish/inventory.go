package redfish

// inventory.go maps the three firmware data sources onto Redfish types.
// All three live at different offsets of the same I2C EEPROM:
//
//	0x0000  UEFI variables    -> Boot (BootNext, BootOrder)
//	0x4000  U-Boot environment -> ComputerSystem identity (fallback)
//	0x6000  SMBIOS tables      -> ComputerSystem identity (preferred),
//	                             plus the type 45 firmware inventory
//
// SMBIOS wins where both carry a value: it is the byte-for-byte source the
// booted OS itself reads, and it carries the UUID, the full product name and
// the processor detail that exist nowhere in the environment. The env
// remains the fallback for a board that has not yet booted this firmware.

import (
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/smbios"
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

// resetTypeSupported reports whether t is one we can service.
func resetTypeSupported(t schemas.ResetType) bool {
	for _, s := range supportedResetTypes {
		if s == t {
			return true
		}
	}
	return false
}

// set assigns v to *dst only when v is non-empty, so a later source can
// overlay an earlier one without blanking fields it does not carry.
func set(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// uptr converts a positive count to the *uint the Redfish schema uses, so an
// absent count is nil (omitted) rather than a misleading zero.
func uptr(v int) *uint {
	if v < 0 {
		v = 0
	}
	u := uint(v)
	return &u
}

// oemNanoKVM returns the system's vendor extension block, creating it on
// first use. It holds the values SMBIOS and the environment carry that have
// no standard ComputerSystem property (DSP0266 §6.4.13).
func oemNanoKVM(sys *ComputerSystem) map[string]any {
	if sys.Oem == nil {
		sys.Oem = Oem{}
	}
	block, ok := sys.Oem["NanoKVM"].(map[string]any)
	if !ok {
		block = map[string]any{odataTypeKey: "#NanoKVM.v1_0_0.ComputerSystem"}
		sys.Oem["NanoKVM"] = block
	}
	return block
}

// applyEnvInventory fills identity fields from the U-Boot environment.
func applyEnvInventory(sys *ComputerSystem) bool {
	inv, err := firmware.GetController().GetInventory()
	if err != nil || len(inv) == 0 {
		return false
	}
	applyEnvInfo(sys, inv)
	return true
}

// applyEnvInfo maps a machine.env inventory map onto the ComputerSystem. It
// is split from applyEnvInventory (which owns the controller read) so the
// mapping can be unit-tested with a fixture map, matching applySMBIOSInfo.
func applyEnvInfo(sys *ComputerSystem, inv map[string]string) {
	set(&sys.Model, inv["board_name"])
	set(&sys.SerialNumber, inv["serial#"])
	set(&sys.Manufacturer, inv["vendor"])
	// SMBIOS type-1 Version is the board revision and lands in SubModel (see
	// applySMBIOSInfo); board_revision is the env's equivalent, so it takes
	// the same slot when the tables are absent.
	set(&sys.SubModel, inv["board_revision"])
	// ComputerSystem has no "FirmwareVersion" property — BiosVersion is the
	// standard slot for the boot firmware's version, and it is what gofish
	// and bmclib read. The pre-migration code emitted FirmwareVersion, which
	// no conformant client looks at.
	set(&sys.BiosVersion, inv["ver"])

	if cpu := inv["cpu"]; cpu != "" {
		sys.ProcessorSummary = &ProcessorSummary{Model: cpu}
	}
	// ethaddr is served through the standard EthernetInterfaces collection
	// (ethernet_interfaces.go), not Oem.
	oem := oemNanoKVM(sys)
	for key, v := range map[string]string{
		// Env-only values with no standard Redfish property anywhere. The
		// web overview reads these from here rather than
		// /api/firmware/inventory.
		"SoC":         inv["soc"],
		"DeviceTree":  inv["fdtfile"],
		"BootMethods": inv["bootmeths"],
	} {
		if v != "" {
			oem[key] = v
		}
	}

	oem["InventorySource"] = "UBootEnv"
}

// applySMBIOSInventory overlays the richer SMBIOS values, leaving whatever
// the environment supplied in place for anything SMBIOS does not carry.
func applySMBIOSInventory(sys *ComputerSystem) bool {
	info, err := smbios.GetStore().Load()
	if err != nil {
		// A blank region just means the host has not booted this firmware
		// yet; an unconfigured store means the operator disabled it. Neither
		// is worth logging on every request.
		if !errors.Is(err, smbios.ErrNoTables) && !errors.Is(err, smbios.ErrNotConfigured) {
			log.Warnf("redfish: reading SMBIOS tables: %v", err)
		}
		return false
	}
	applySMBIOSInfo(sys, info)
	return true
}

// applySMBIOSInfo maps a parsed SMBIOS Info onto the ComputerSystem. It is
// split from applySMBIOSInventory (which owns the store read) so the mapping
// can be unit-tested with a fixture Info.
func applySMBIOSInfo(sys *ComputerSystem, info *smbios.Info) {
	set(&sys.Manufacturer, info.Manufacturer)
	set(&sys.Model, info.Product)
	set(&sys.SerialNumber, info.Serial)
	set(&sys.SKU, info.SKU)
	set(&sys.UUID, info.UUID)
	set(&sys.BiosVersion, info.BIOSVersion)
	// SMBIOS type-1 Version is the board revision ("1.1"). Redfish has no
	// ComputerSystem.Version; SubModel is the closest standard property.
	set(&sys.SubModel, info.Version)

	if info.CPUVersion != "" || info.CPUCores > 0 {
		ps := &ProcessorSummary{Model: info.CPUVersion}
		if info.CPUCores > 0 {
			ps.CoreCount = uptr(info.CPUCores)
			ps.Count = uptr(1) // the RPi5 is a single-socket part
		}
		if info.CPUThreads > 0 {
			ps.LogicalProcessorCount = uptr(info.CPUThreads)
		}
		sys.ProcessorSummary = ps
	}

	// Memory (SMBIOS type 16/17) -> the standard MemorySummary. The total is
	// the sum of installed modules; the RPi 5 SoC-package DRAM is not mirrored.
	if info.MemoryTotalMB > 0 {
		gib := float64(info.MemoryTotalMB) / 1024
		sys.MemorySummary = &MemorySummary{
			TotalSystemMemoryGiB: &gib,
			MemoryMirroring:      schemas.NoneMemoryMirroring,
			Status:               &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		}
	}

	// Only values with NO standard Redfish home remain under Oem. The rest
	// moved to their DSP2046 resources: per-module memory detail and ECC →
	// the Memory collection (memory.go); processor manufacturer/part/max
	// speed → the Processor resource (processors.go); board identity → the
	// Chassis (chassis.go); BIOS vendor/date → the UpdateService's BIOS
	// SoftwareInventory (update_service.go); the MAC → EthernetInterfaces.
	oem := oemNanoKVM(sys)
	oem["InventorySource"] = "SMBIOS"
	for key, v := range map[string]string{
		// SMBIOS type-1 Family and the spec version have no ComputerSystem
		// property in any schema revision.
		"Family":        info.Family,
		"SMBIOSVersion": info.SMBIOSVersion,
		// The boot firmware (rpi-eeprom) is a separate component from
		// BiosVersion's U-Boot (SMBIOS type 0): this is the loader that
		// runs before it (type 45). Its standard exposure is the
		// TrustedComponent's SoftwareInventory (trusted_components.go);
		// the version stays here too for the web overview's one-call read.
		"BootFirmwareVersion": info.BootFirmwareVersion,
	} {
		if v != "" {
			oem[key] = v
		}
	}
	// SMBIOS type 45 — every firmware image the host reports, raw. The
	// running loader's standard model is the TrustedComponent +
	// SoftwareInventory; the full list has no per-component standard
	// resource yet.
	if len(info.FirmwareInventory) > 0 {
		oem["FirmwareInventory"] = info.FirmwareInventory
	}
	// Populated-slot count (type 16) and system slots (type 9): Redfish
	// homes these under Chassis PCIeSlots/MemoryDomains, which this board
	// (one soldered package, one FPC connector) does not warrant.
	if info.MemorySlots > 0 {
		oem["MemorySlots"] = info.MemorySlots
	}
	if len(info.Slots) > 0 {
		oem["Slots"] = info.Slots
	}
}

// readBoot reports the current boot override as a Redfish Boot block.
//
// When the UEFI variable store (I2C EEPROM offset 0) is available it is
// authoritative: a pending BootNext reads back as "Once", and a continuous
// override is a plain BootOrder reorder that reads back as "Disabled".
// Otherwise we fall back to the legacy U-Boot env boot_targets semantics.
func readBoot() Boot {
	boot := Boot{
		BootSourceOverrideTarget:  schemas.NoneBootSource,
		BootSourceOverrideEnabled: schemas.DisabledBootSourceOverrideEnabled,
		// The RPi5 firmware path is UEFI-only, so there is no Legacy toggle
		// to honour. We still report the property so PATCH echoes match.
		BootSourceOverrideMode: schemas.UEFIBootSourceOverrideMode,
		AllowableTargets:       supportedBootSources,
		AllowableEnabled:       supportedOverrideEnabled,
		AllowableModes:         []schemas.BootSourceOverrideMode{schemas.UEFIBootSourceOverrideMode},
	}

	if mgr := efivars.GetManager(); mgr.Available() {
		if t, e, err := mgr.BootSourceOverride(); err == nil {
			if t != efivars.TargetUnknown {
				boot.BootSourceOverrideTarget = schemas.BootSource(t)
			}
			if e != "" {
				boot.BootSourceOverrideEnabled = schemas.BootSourceOverrideEnabled(e)
			}
		} else {
			log.Warnf("redfish: read boot override failed: %v", err)
		}
		if order, err := mgr.BootOrder(); err == nil {
			boot.BootOrder = make([]string, len(order))
			for i, id := range order {
				boot.BootOrder[i] = fmt.Sprintf("Boot%04X", id)
			}
		}
		return boot
	}

	// Legacy env fallback — persistent takes precedence over once.
	fwCtrl := firmware.GetController()
	if targets, err := fwCtrl.GetBootTarget(); err == nil && targets != "" {
		if rt, ok := firmware.UBootToRedfish[targets]; ok {
			boot.BootSourceOverrideTarget = schemas.BootSource(rt)
			boot.BootSourceOverrideEnabled = schemas.ContinuousBootSourceOverrideEnabled
			return boot
		}
	}
	if targets, err := fwCtrl.GetOnceBootTarget(); err == nil && targets != "" {
		if rt, ok := firmware.UBootToRedfish[targets]; ok {
			boot.BootSourceOverrideTarget = schemas.BootSource(rt)
			boot.BootSourceOverrideEnabled = schemas.OnceBootSourceOverrideEnabled
			return boot
		}
	}
	return boot
}

// setBootOverride applies a boot source override. When the UEFI variable
// store is available it drives the EFI boot manager directly: BootNext for
// Once, a BootOrder reorder for Continuous. Otherwise it falls back to the
// U-Boot env boot_targets.
func setBootOverride(target schemas.BootSource, enabled schemas.BootSourceOverrideEnabled) error {
	if mgr := efivars.GetManager(); mgr.Available() {
		// BiosSetup has no Boot#### entry to point at; treat it as a no-op,
		// matching the env path which maps it to an empty boot_targets.
		if target == schemas.BiosSetupBootSource {
			return nil
		}
		return mgr.SetBootSourceOverride(efivars.BootTarget(target),
			enabled != schemas.ContinuousBootSourceOverrideEnabled)
	}

	fwCtrl := firmware.GetController()
	ubootTargets := firmware.RedfishToUBoot[string(target)]
	if enabled == schemas.ContinuousBootSourceOverrideEnabled {
		return fwCtrl.SetBootTarget(ubootTargets)
	}
	return fwCtrl.SetBootTargetOnce(ubootTargets)
}

// clearBootOverride removes any pending boot source override.
func clearBootOverride() {
	if mgr := efivars.GetManager(); mgr.Available() {
		if err := mgr.ClearBootSourceOverride(); err != nil {
			log.Warnf("redfish: clear BootNext failed: %v", err)
		}
		return
	}

	fwCtrl := firmware.GetController()
	if err := fwCtrl.SetBootTarget(""); err != nil {
		log.Warnf("redfish: clear persistent boot failed: %v", err)
	}
	if err := fwCtrl.SetBootTargetOnce(""); err != nil {
		log.Warnf("redfish: clear once boot failed: %v", err)
	}
}
