package redfish

import (
	"testing"

	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/smbios"
)

// readBoot converts an efivars.BootTarget straight into a schemas.BootSource,
// and setBootOverride converts back. That is only sound while every efivars
// target is spelled exactly like a gofish BootSource — otherwise we'd emit an
// enum value no client can read. Pin the invariant here rather than discover
// it when someone adds a target.
func TestEFIBootTargetsAreValidBootSources(t *testing.T) {
	for _, target := range []efivars.BootTarget{
		efivars.TargetPxe,
		efivars.TargetHdd,
		efivars.TargetCd,
		efivars.TargetUefiHttp,
	} {
		if !bootSourceSupported(schemas.BootSource(target)) {
			t.Errorf("efivars.BootTarget %q is not a supported schemas.BootSource; "+
				"readBoot would emit an invalid enum", target)
		}
	}

	// The reverse direction: everything we accept must round-trip to a target
	// efivars understands (BiosSetup and None are handled before conversion).
	for _, src := range supportedBootSources {
		if src == schemas.NoneBootSource || src == schemas.BiosSetupBootSource {
			continue
		}
		switch efivars.BootTarget(src) {
		case efivars.TargetPxe, efivars.TargetHdd, efivars.TargetCd, efivars.TargetUefiHttp:
		default:
			t.Errorf("supported BootSource %q has no matching efivars.BootTarget", src)
		}
	}
}

// The env fallback path maps through firmware.UBootToRedfish; its values must
// also be valid BootSources for the same reason.
func TestUBootToRedfishValuesAreValidBootSources(t *testing.T) {
	for ubootTarget, redfishName := range firmware.UBootToRedfish {
		if !bootSourceSupported(schemas.BootSource(redfishName)) {
			t.Errorf("UBootToRedfish[%q] = %q, which is not a supported BootSource",
				ubootTarget, redfishName)
		}
	}
	// And every source we advertise must be settable via the env fallback.
	for _, src := range supportedBootSources {
		if _, ok := firmware.RedfishToUBoot[string(src)]; !ok {
			t.Errorf("supported BootSource %q missing from firmware.RedfishToUBoot; "+
				"the env fallback would silently set an empty boot_targets", src)
		}
	}
}

// set() implements the overlay: SMBIOS layers over the env without blanking
// fields it doesn't carry.
func TestSetOnlyOverwritesWithNonEmpty(t *testing.T) {
	got := "from-env"
	set(&got, "")
	if got != "from-env" {
		t.Errorf("empty value clobbered the field: got %q", got)
	}
	set(&got, "from-smbios")
	if got != "from-smbios" {
		t.Errorf("non-empty value did not overlay: got %q", got)
	}
}

func TestOemNanoKVMIsCreatedOnceAndTyped(t *testing.T) {
	var sys ComputerSystem

	a := oemNanoKVM(&sys)
	a["MACAddress"] = "d8:3a:dd:00:00:01"
	b := oemNanoKVM(&sys)
	b["InventorySource"] = "SMBIOS"

	if len(sys.Oem) != 1 {
		t.Fatalf("Oem has %d blocks, want 1", len(sys.Oem))
	}
	block, ok := sys.Oem["NanoKVM"].(map[string]any)
	if !ok {
		t.Fatalf("Oem.NanoKVM is %T", sys.Oem["NanoKVM"])
	}
	// Both writes must land in the same block.
	if block["MACAddress"] != "d8:3a:dd:00:00:01" || block["InventorySource"] != "SMBIOS" {
		t.Errorf("block lost a value: %v", block)
	}
	if block["@odata.type"] != "#NanoKVM.v1_0_0.ComputerSystem" {
		t.Errorf("Oem block missing @odata.type, got %v", block["@odata.type"])
	}
}

// The web overview reads the Server Information card entirely off
// /Systems/1, so every machine.env value it shows must survive the mapping —
// including the four with no standard ComputerSystem property, which land in
// Oem.NanoKVM. A board that never boots the SMBIOS-writing firmware has only
// this path, so a dropped key here is a blank row in the UI.
func TestApplyEnvInfo(t *testing.T) {
	var sys ComputerSystem
	applyEnvInfo(&sys, map[string]string{
		"board_name":     "rpi5",
		"vendor":         "Raspberry Pi",
		"serial#":        "10000000abcdef01",
		"board_revision": "1.1",
		"ver":            "U-Boot 2026.04",
		"cpu":            "armv8",
		"soc":            "bcm2712",
		"ethaddr":        "d8:3a:dd:00:11:22",
		"fdtfile":        "broadcom/bcm2712-rpi-5-b.dtb",
		"bootmeths":      "efi pxe dhcp",
	})

	for name, got := range map[string]string{
		"Model":        sys.Model,
		"Manufacturer": sys.Manufacturer,
		"SerialNumber": sys.SerialNumber,
		"SubModel":     sys.SubModel,
		"BiosVersion":  sys.BiosVersion,
	} {
		if got == "" {
			t.Errorf("%s not set from env", name)
		}
	}
	// board_revision has no ComputerSystem.Version to land in; SubModel is
	// the same slot SMBIOS type-1 Version uses.
	if sys.SubModel != "1.1" {
		t.Errorf("SubModel = %q, want 1.1 (board_revision)", sys.SubModel)
	}
	if sys.ProcessorSummary == nil || sys.ProcessorSummary.Model != "armv8" {
		t.Errorf("ProcessorSummary = %+v, want Model armv8", sys.ProcessorSummary)
	}

	oem := oemNanoKVM(&sys)
	for key, want := range map[string]any{
		"SoC":             "bcm2712",
		"DeviceTree":      "broadcom/bcm2712-rpi-5-b.dtb",
		"BootMethods":     "efi pxe dhcp",
		"InventorySource": "UBootEnv",
	} {
		if oem[key] != want {
			t.Errorf("Oem[%q] = %v, want %v", key, oem[key], want)
		}
	}
	// The MAC's standard home is the EthernetInterfaces collection
	// (ethernet_interfaces.go); it must no longer leak into Oem.
	if _, present := oem["MACAddress"]; present {
		t.Error("Oem[MACAddress] present; the MAC belongs to EthernetInterfaces")
	}
}

// An env that carries only some keys must not emit empty Oem entries — the
// overview distinguishes "absent" (em-dash) from a real blank value.
func TestApplyEnvInfoOmitsAbsentKeys(t *testing.T) {
	var sys ComputerSystem
	applyEnvInfo(&sys, map[string]string{"board_name": "rpi5"})

	oem := oemNanoKVM(&sys)
	for _, key := range []string{"SoC", "DeviceTree", "BootMethods"} {
		if _, present := oem[key]; present {
			t.Errorf("Oem[%q] present for an env that does not carry it", key)
		}
	}
}

// SMBIOS is overlaid on top of the env and only writes non-empty values, so
// the env-only keys the overview reads must survive the overlay.
func TestSMBIOSOverlayKeepsEnvOnlyOemKeys(t *testing.T) {
	var sys ComputerSystem
	applyEnvInfo(&sys, map[string]string{
		"board_name": "rpi5",
		"soc":        "bcm2712",
		"fdtfile":    "broadcom/bcm2712-rpi-5-b.dtb",
		"bootmeths":  "efi pxe dhcp",
		"ethaddr":    "d8:3a:dd:00:11:22",
	})
	applySMBIOSInfo(&sys, &smbios.Info{Manufacturer: "Raspberry Pi", Product: "Raspberry Pi 5 Model B"})

	oem := oemNanoKVM(&sys)
	for _, key := range []string{"SoC", "DeviceTree", "BootMethods"} {
		if oem[key] == nil || oem[key] == "" {
			t.Errorf("Oem[%q] lost when SMBIOS was overlaid", key)
		}
	}
	// The source label, by contrast, must report the winner.
	if oem["InventorySource"] != "SMBIOS" {
		t.Errorf("InventorySource = %v, want SMBIOS", oem["InventorySource"])
	}
}

// applySMBIOSInfo must project the SMBIOS memory tables onto the standard
// ComputerSystem.MemorySummary. Per-module detail and ECC now live on the
// Memory collection (memory.go); only the values with no standard home
// anywhere (populated-slot count, system slots) may remain under Oem.
func TestApplySMBIOSInfoMemory(t *testing.T) {
	var sys ComputerSystem
	info := &smbios.Info{
		MemoryTotalMB:         16384,
		MemoryErrorCorrection: "Single-bit ECC",
		MemorySlots:           1,
		Memory: []smbios.MemoryModule{{
			Locator:      "P0",
			SizeMB:       16384,
			Type:         "LPDDR4",
			FormFactor:   "Row of chips",
			SpeedMTs:     4267,
			Manufacturer: "Micron",
			PartNumber:   "MT53E2G32",
		}},
		Slots: []string{"PCIe"},
	}

	applySMBIOSInfo(&sys, info)

	if sys.MemorySummary == nil {
		t.Fatal("MemorySummary not set")
	}
	if sys.MemorySummary.TotalSystemMemoryGiB == nil {
		t.Fatal("TotalSystemMemoryGiB nil")
	}
	if got := *sys.MemorySummary.TotalSystemMemoryGiB; got != 16 {
		t.Errorf("TotalSystemMemoryGiB = %v, want 16", got)
	}
	if sys.MemorySummary.MemoryMirroring != schemas.NoneMemoryMirroring {
		t.Errorf("MemoryMirroring = %q, want None", sys.MemorySummary.MemoryMirroring)
	}
	if sys.MemorySummary.Status == nil ||
		sys.MemorySummary.Status.State != schemas.EnabledState ||
		sys.MemorySummary.Status.Health != schemas.OKHealth {
		t.Errorf("Status = %+v, want Enabled/OK", sys.MemorySummary.Status)
	}

	oem := oemNanoKVM(&sys)
	if oem["MemorySlots"] != 1 {
		t.Errorf("Oem[MemorySlots] = %v, want 1", oem["MemorySlots"])
	}
	if slots, ok := oem["Slots"].([]string); !ok || len(slots) != 1 || slots[0] != "PCIe" {
		t.Errorf("Oem[Slots] = %v, want [PCIe]", oem["Slots"])
	}
	// Everything with a standard Memory-resource member must be gone.
	for _, key := range []string{"MemoryErrorCorrection", "MemoryType", "MemorySpeedMTs", "MemoryDevices"} {
		if _, present := oem[key]; present {
			t.Errorf("Oem[%q] present; this detail belongs to the Memory collection", key)
		}
	}
}

// memoryResource must project an SMBIOS type-17 module onto the standard
// Memory resource, including the enum translations.
func TestMemoryResourceMapping(t *testing.T) {
	m := smbios.MemoryModule{
		Locator:            "P0 CH0",
		SizeMB:             16384,
		Type:               "LPDDR4",
		SpeedMTs:           4267,
		ConfiguredSpeedMTs: 4267,
		Manufacturer:       "Micron",
		PartNumber:         "MT53E2G32",
		SerialNumber:       "0000000",
		DataWidthBits:      32,
		TotalWidthBits:     32,
	}
	id := memoryID(0, m)
	if id != "P0CH0" {
		t.Errorf("memoryID = %q, want locator-derived P0CH0", id)
	}
	res := memoryResource(id, m, "Single-bit ECC")
	if res.MemoryDeviceType != "LPDDR4_SDRAM" {
		t.Errorf("MemoryDeviceType = %q, want LPDDR4_SDRAM", res.MemoryDeviceType)
	}
	if res.ErrorCorrection != schemas.SingleBitECCErrorCorrection {
		t.Errorf("ErrorCorrection = %q, want SingleBitECC", res.ErrorCorrection)
	}
	if res.CapacityMiB == nil || *res.CapacityMiB != 16384 {
		t.Errorf("CapacityMiB = %v, want 16384", res.CapacityMiB)
	}
	if res.OperatingSpeedMhz == nil || *res.OperatingSpeedMhz != 4267 {
		t.Errorf("OperatingSpeedMhz = %v, want 4267", res.OperatingSpeedMhz)
	}
	if res.DeviceLocator != "P0 CH0" || res.Manufacturer != "Micron" || res.PartNumber != "MT53E2G32" {
		t.Errorf("identity fields wrong: %+v", res)
	}
}

// A module with no locator falls back to a positional Id.
func TestMemoryIDFallback(t *testing.T) {
	if id := memoryID(2, smbios.MemoryModule{}); id != "DIMM2" {
		t.Errorf("memoryID = %q, want DIMM2", id)
	}
}

// A board that carries no memory tables (older firmware, blank region) must not
// invent a MemorySummary or any memory Oem keys.
func TestApplySMBIOSInfoNoMemory(t *testing.T) {
	var sys ComputerSystem
	applySMBIOSInfo(&sys, &smbios.Info{Manufacturer: "Raspberry Pi"})

	if sys.MemorySummary != nil {
		t.Errorf("MemorySummary = %+v, want nil", sys.MemorySummary)
	}
	oem := oemNanoKVM(&sys)
	for _, key := range []string{"MemoryType", "MemorySlots", "MemoryDevices", "Slots"} {
		if _, present := oem[key]; present {
			t.Errorf("Oem[%q] present with no memory tables", key)
		}
	}
}

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
