package redfish

// bios_catalog.go is the platform's BIOS attribute vocabulary, compiled in.
//
// The host owns this vocabulary and publishes it as an AttributeRegistry —
// RedfishPlatformConfigDxe harvests every Setup question tagged with an
// x-UEFI-redfish-Bios.v1_1_0 configure-language string out of the HII database
// and the Bios feature driver PUTs the result. When that document is present
// this file contributes nothing.
//
// It is not always present. The registry arrives late in the host-interface
// exchange, and a host that has reported its attributes but not yet its
// registry leaves the BMC describing every one of them by guessing a type from
// the JSON value it holds. What that guess cannot recover:
//
//   - an Enumeration is indistinguishable from a String. "Automatic" is a
//     string, so FanMode draws a free-text box rather than the dropdown the
//     firmware will accept, and any typo stages cleanly and is dropped on
//     the floor at the next boot;
//   - bounds do not exist at all. A trip point takes 200C, an IPv4 field takes
//     forty characters, and both are rejected by the host rather than here;
//   - a value whose JSON shape disagrees with its question's type carries that
//     disagreement into the staged object, and biosCoerce — which reads Type —
//     has nothing better to go on.
//
// So the same table fixes the control and the coercion, which is why it lives
// beside the view rather than in the UI layer.
//
// Transcribed from rpi5-uefi-build, at
// meta-rpi5-uefi/recipes-bsp/edk2-platforms/files/edk2-platforms/Platform/
// RaspberryPi{,/RPi5}/Drivers/<Driver>/:
//
//   - <Driver>Map.uni gives the attribute name (last segment of the
//     /Bios/Attributes/... pointer) and, for a oneof, the enum value names —
//     those strings are the vocabulary the BMC reads and writes, deliberately
//     kept as identifiers while en-US carries the display text;
//   - <Driver>Hii.vfr gives the opcode (oneof / checkbox / numeric / string)
//     and its bounds;
//   - the en-US strings give DisplayName and the option labels.
//
// Two rules keep it from doing harm when it is wrong:
//
//   - the host always wins, field by field. A registry that names a value this
//     table has never heard of is a firmware newer than this file, and the
//     catalog must never overrule it. Only fields the registry left empty are
//     filled.
//   - it describes; it does not invent. An attribute the host has never
//     reported produces no row, the same way EthernetInterfaces stays empty
//     until the first report lands.
//
// TestBiosCatalogCoversEveryPlatformFormset pins the roster against the
// firmware's formsets, so a new *Map.uni upstream fails here rather than
// quietly rendering one more text box.

// biosCatalogEntry describes one attribute the way a registry entry would.
// The field set is deliberately the subset of biosRegEntry that changes what
// an editor draws or what biosCoerce produces — no help text, because a
// paraphrase of the firmware's own wording is a thing to keep in step for no
// benefit, and the row already falls back to showing the raw attribute key.
type biosCatalogEntry struct {
	DisplayName string
	Type        string
	MenuPath    string
	Order       int
	Options     []BiosOption
	LowerBound  *int64
	UpperBound  *int64
	MinLength   *int64
	MaxLength   *int64
}

// Menu paths mirror the Setup screens the questions live on, so a page built
// from this table navigates the way the firmware's own menus do.
//
// "ACPI / Device Tree" is one screen, not two: menuSegments treats a slash
// with whitespace beside it as part of the name rather than a separator,
// precisely for EDK2 titles shaped like this one.
const (
	biosMenuFan        = "./Active Cooler"
	biosMenuSystemTbl  = "./ACPI / Device Tree"
	biosMenuPCIe       = "./PCI Express"
	biosMenuBootloader = "./Bootloader EEPROM"
	biosMenuMemAttr    = "./EFI Memory Attribute Protocol"
	biosMenuSecureBoot = "./Secure Boot"
	biosMenuPower      = "./Power Profile"
)

// goconst counts these literals across the whole package and reports them
// here, at the only non-test file they appear in — the repo's lint config
// already forgives the same duplication in the tests that supply most of the
// occurrences. Extracting them would be wrong twice over. The literals are
// this file's product: it earns its keep by being readable side by side with
// *Map.uni and *Hii.vfr, and `{Value: ethModeDhcp}` cannot be checked against
// the firmware without jumping somewhere else. And goconst's "such constant
// already exists" hints are traps here — schemaNameSecureBoot is a Redfish
// schema name, redfishDisabled is a Status.State member, and this package
// carries two vocabularies that happen to share spellings. Binding them would
// let a future Redfish rename silently change which firmware question the BMC
// describes.
//
//nolint:goconst // firmware vocabulary, kept literal to stay diffable against the .uni/.vfr sources
var biosCatalog = map[string]biosCatalogEntry{
	// ── FanConfigDxe ────────────────────────────────────────────────────
	"FanMode": {
		DisplayName: "Fan Control Mode", Type: BiosTypeEnumeration,
		MenuPath: biosMenuFan, Order: 1,
		Options: []BiosOption{
			{Value: "Automatic", Label: "Automatic"},
			{Value: "FixedSpeed", Label: "Fixed Speed"},
			{Value: "CustomTripPoints", Label: "Custom Trip Points"},
		},
	},
	"FanFixedLevel": {
		DisplayName: "Fixed Fan Level", Type: BiosTypeEnumeration,
		MenuPath: biosMenuFan, Order: 2,
		Options: []BiosOption{
			{Value: "Level0", Label: "Level 0 (fan off)"},
			{Value: "Level1", Label: "Level 1 (29% duty)"},
			{Value: "Level2", Label: "Level 2 (49% duty)"},
			{Value: "Level3", Label: "Level 3 (69% duty)"},
			{Value: "Level4", Label: "Level 4 (98% duty)"},
		},
	},
	"FanTrip1C": fanTrip("Level 1 Trip Temperature (C)", 3),
	"FanTrip2C": fanTrip("Level 2 Trip Temperature (C)", 4),
	"FanTrip3C": fanTrip("Level 3 Trip Temperature (C)", 5),
	"FanTrip4C": fanTrip("Level 4 Trip Temperature (C)", 6),

	// ── RpiPlatformDxe: ACPI / Device Tree ──────────────────────────────
	"SystemTableMode": {
		DisplayName: "System Table Mode", Type: BiosTypeEnumeration,
		MenuPath: biosMenuSystemTbl, Order: 1,
		Options: []BiosOption{
			{Value: "Acpi", Label: "ACPI"},
			{Value: "DeviceTree", Label: "Device Tree"},
			{Value: "AcpiAndDeviceTree", Label: "Both"},
		},
	},
	"AcpiSdCompatMode": {
		DisplayName: "SD Compatibility Mode", Type: BiosTypeEnumeration,
		MenuPath: biosMenuSystemTbl, Order: 2,
		Options: []BiosOption{
			{Value: "BrcmstbAndBaytrail", Label: "BRCMSTB + Bay Trail"},
			{Value: "FullBaytrail", Label: "Full Bay Trail"},
		},
	},
	"AcpiSdLimitUhs": {
		DisplayName: "Limit UHS-I Modes", Type: BiosTypeBoolean,
		MenuPath: biosMenuSystemTbl, Order: 3,
	},
	"AcpiPcieEcamCompatMode": {
		DisplayName: "PCIe ECAM Compatibility Mode", Type: BiosTypeEnumeration,
		MenuPath: biosMenuSystemTbl, Order: 4,
		Options: []BiosOption{
			{Value: "NxpMx6AndDen0115", Label: "Auto (NXPMX6 / Arm DEN0115)"},
			{Value: "NxpMx6AndGraviton", Label: "Auto (NXPMX6 / AMAZON GRAVITON)"},
			{Value: "Den0115", Label: "Arm DEN0115"},
			{Value: "NxpMx6", Label: "NXPMX6"},
			{Value: "Graviton", Label: "AMAZON GRAVITON"},
		},
	},
	"AcpiPcie32BitBarSpaceSizeMB": {
		DisplayName: "32-bit BAR Space Preferred Size (MB)", Type: BiosTypeInteger,
		MenuPath: biosMenuSystemTbl, Order: 5,
		// ConfigTable.h: MAXIMUM is PCI_RESERVED_MEM32_SIZE / 1024 / 1024.
		LowerBound: int64p(0), UpperBound: int64p(1024),
	},

	// ── RpiPlatformDxe: PCI Express ─────────────────────────────────────
	// Both questions sit under a "PCIe Controller #1" subtitle in Setup,
	// which a flat attribute list loses — hence the prefix, or the page would
	// show a switch labelled just "Enabled".
	"Pcie1Enabled": {
		DisplayName: "Controller #1 Enabled", Type: BiosTypeBoolean,
		MenuPath: biosMenuPCIe, Order: 1,
	},
	"Pcie1MaxLinkSpeed": {
		DisplayName: "Controller #1 Link Speed", Type: BiosTypeEnumeration,
		MenuPath: biosMenuPCIe, Order: 2,
		Options: []BiosOption{
			{Value: "Gen1", Label: "Gen 1 (2.5 GT/s)"},
			{Value: "Gen2", Label: "Gen 2 (5 GT/s)"},
			{Value: "Gen3", Label: "Gen 3 (8 GT/s)"},
		},
	},

	// ── BootloaderConfigDxe ─────────────────────────────────────────────
	// Read-mostly: these reach the BlCfg variable, not the EEPROM, and
	// BootloaderConfigDxe re-seeds that variable from the live blconfig every
	// boot. Only the interactive "stage update" action in Setup writes SPI.
	"BlBootOrder": {
		DisplayName: "Boot Order (BOOT_ORDER)", Type: BiosTypeString,
		MenuPath: biosMenuBootloader, Order: 1,
		MinLength: int64p(1), MaxLength: int64p(11),
	},
	"BlBootUart":       blToggle("Bootloader UART Log (BOOT_UART)", 2),
	"BlPowerOffOnHalt": blToggle("Power Off On Halt (POWER_OFF_ON_HALT)", 3),
	"BlWakeOnGpio":     blToggle("Wake On GPIO (WAKE_ON_GPIO)", 4),
	"BlPsuMaxCurrent": {
		DisplayName: "PSU Max Current (PSU_MAX_CURRENT)", Type: BiosTypeEnumeration,
		MenuPath: biosMenuBootloader, Order: 5,
		Options: []BiosOption{
			{Value: "Auto", Label: "Auto (negotiate)"},
			{Value: "Milliamps3000", Label: "3000 mA"},
			{Value: "Milliamps5000", Label: "5000 mA"},
		},
	},

	// ── MemoryAttributeManagerDxe ───────────────────────────────────────
	"MemoryAttributeProtocol": {
		DisplayName: "EFI Memory Attribute Protocol", Type: BiosTypeBoolean,
		MenuPath: biosMenuMemAttr, Order: 1,
	},

	// ── SecureBootToggleDxe ─────────────────────────────────────────────
	// Present only on RPI5_SECURE_BOOT=1 builds. A name-keyed table needs no
	// gate for that: a host that does not publish the attribute gets no row.
	"SecureBoot": {
		DisplayName: "Secure Boot", Type: BiosTypeBoolean,
		MenuPath: biosMenuSecureBoot, Order: 1,
	},

	// ── RpiScmiConfigDxe ────────────────────────────────────────────────
	"PowerProfile": {
		DisplayName: "Power Profile", Type: BiosTypeEnumeration,
		MenuPath: biosMenuPower, Order: 1,
		Options: []BiosOption{
			{Value: "Balanced", Label: "Balanced"},
			{Value: "Quiet", Label: "Quiet"},
			{Value: "Cool", Label: "Cool"},
			{Value: "Manual", Label: "Manual (use Active Cooler page)"},
		},
	},
}

// fanTrip builds one of the four identical trip-point questions.
func fanTrip(label string, order int) biosCatalogEntry {
	return biosCatalogEntry{
		DisplayName: label, Type: BiosTypeInteger,
		MenuPath: biosMenuFan, Order: order,
		LowerBound: int64p(30), UpperBound: int64p(90),
	}
}

// blToggle builds one of the three Disabled/Enabled bootloader questions.
// They are oneofs in the VFR rather than checkboxes, so they stay
// Enumerations here: the firmware reads the strings, not a JSON boolean.
//
// The literals below are the bootloader's own vocabulary and must not be
// folded into redfishDisabled, which is the DMTF Status.State member — see the
// note on biosCatalog.
//
//nolint:goconst // bootloader enum values, distinct from the identically spelled Redfish ones
func blToggle(label string, order int) biosCatalogEntry {
	return biosCatalogEntry{
		DisplayName: label, Type: BiosTypeEnumeration,
		MenuPath: biosMenuBootloader, Order: order,
		Options: []BiosOption{
			{Value: "Disabled", Label: "Disabled"},
			{Value: "Enabled", Label: "Enabled"},
		},
	}
}

func int64p(n int64) *int64 { return &n }

// applyBiosCatalog fills in whatever the host's registry left undescribed and
// reports whether it contributed anything.
//
// Gap-filling, never overriding: each field is taken only when the registry
// said nothing about it. That is what keeps a firmware newer than this file
// working — a registry listing a link speed this table has never heard of
// keeps its own list, and the value stages.
//
// Callers on the unregistered path leave Type empty rather than pre-filling
// the inferred one, so a catalog hit wins over the guess and a miss can fall
// back to it.
func applyBiosCatalog(a *BiosAttribute) bool {
	e, ok := biosCatalog[a.Name]
	if !ok {
		return false
	}
	used := false
	take := func(gap bool, fill func()) {
		if gap {
			fill()
			used = true
		}
	}

	take(a.Type == "", func() { a.Type = e.Type })
	take(a.DisplayName == "", func() { a.DisplayName = e.DisplayName })
	// An unregistered attribute is parked in the leftovers bucket before this
	// runs; a described one belongs on the menu its Setup screen lives on.
	take(a.MenuPath == "" || a.MenuPath == unregisteredMenuPath, func() {
		a.MenuPath = e.MenuPath
		if a.Order == 0 {
			a.Order = e.Order
		}
	})
	// An Enumeration with no allowable values renders free text and validates
	// nothing, so a registry that names the attribute without listing its
	// values has still left the gap this fills.
	take(a.Type == BiosTypeEnumeration && len(a.Options) == 0 && len(e.Options) > 0,
		func() { a.Options = e.Options })
	take(a.Type == BiosTypeInteger && a.LowerBound == nil && e.LowerBound != nil,
		func() { a.LowerBound = e.LowerBound })
	take(a.Type == BiosTypeInteger && a.UpperBound == nil && e.UpperBound != nil,
		func() { a.UpperBound = e.UpperBound })
	take(a.Type == BiosTypeString && a.MinLength == nil && e.MinLength != nil,
		func() { a.MinLength = e.MinLength })
	take(a.Type == BiosTypeString && a.MaxLength == nil && e.MaxLength != nil,
		func() { a.MaxLength = e.MaxLength })

	return used
}
