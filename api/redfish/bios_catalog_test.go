package redfish

// bios_catalog_test.go covers the compiled-in platform vocabulary that
// describes attributes before the host publishes its AttributeRegistry.
//
// The visible symptom it fixes is a wall of text boxes: with no registry every
// reported attribute has its type guessed from the JSON value it holds, so
// "Dhcp" is a String and EthIp4Mode draws a free-text field instead of the
// three-value dropdown the firmware actually accepts.
//
// The invisible one matters more. biosCoerce reads Type off the attribute, so
// a guessed type carries into the staged object and the host's Bios v1_1_0
// client reads back a value of the wrong JSON shape. Nothing reports an error;
// the setting simply does not take. The catalog fixes the control and the
// coercion from one place, which is why it lives here and not in the UI layer.
//
// The host always wins. A registry that disagrees with this table is the
// firmware describing itself, and a firmware update that adds a value must
// never be overruled by a stale compiled-in list.

import (
	"testing"
)

// TestBiosCatalogDescribesUnregisteredEnum is the reported case: the host has
// reported EthIp4Mode but not published a registry.
func TestBiosCatalogDescribesUnregisteredEnum(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{"EthIp4Mode": "Dhcp"})

	a := attrByName(t, BiosSnapshot(), "EthIp4Mode")

	if a.Type != BiosTypeEnumeration {
		t.Errorf("type = %q, want %q — a String renders a text box, not a dropdown",
			a.Type, BiosTypeEnumeration)
	}
	if !a.Cataloged {
		t.Error("Cataloged = false; the row cannot tell the operator where its description came from")
	}
	if a.Registered {
		t.Error("Registered = true; the host published no registry")
	}
	want := []string{"Unmanaged", "Dhcp", "Static"}
	if got := optionValues(a); !equalStrings(got, want) {
		t.Errorf("options = %v, want %v (EthConfigDxeMap.uni)", got, want)
	}
	if a.MenuPath == unregisteredMenuPath {
		t.Errorf("menu = %q; a described attribute does not belong in the leftovers bucket",
			a.MenuPath)
	}
	if a.DisplayName == "" {
		t.Error("no DisplayName; the rail would show the raw key")
	}
}

// A checkbox question and a bounded numeric are the two the guessed type gets
// wrong in a way the host can act on.
func TestBiosCatalogTypesBooleansAndBoundedIntegers(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{
		// Reported as strings, which is exactly what a host that has not yet
		// published its registry looks like on the wire.
		"Pcie1Enabled": "false",
		"FanTrip1C":    "50",
	})

	v := BiosSnapshot()

	if got := attrByName(t, v, "Pcie1Enabled").Type; got != BiosTypeBoolean {
		t.Errorf("Pcie1Enabled type = %q, want Boolean (HII checkbox)", got)
	}

	trip := attrByName(t, v, "FanTrip1C")
	if trip.Type != BiosTypeInteger {
		t.Errorf("FanTrip1C type = %q, want Integer", trip.Type)
	}
	if trip.LowerBound == nil || *trip.LowerBound != 30 {
		t.Errorf("FanTrip1C lower bound = %v, want 30 (FanConfigHii.vfr)", trip.LowerBound)
	}
	if trip.UpperBound == nil || *trip.UpperBound != 90 {
		t.Errorf("FanTrip1C upper bound = %v, want 90 (FanConfigHii.vfr)", trip.UpperBound)
	}
}

// The reason the catalog lives in the domain layer rather than the UI:
// biosCoerce reads Type, so the table that picks the control also decides what
// JSON type reaches the staged object.
//
// The inputs here are reported as strings on purpose. That is the shape a host
// with no registry produces for every oneof-backed question, and biosToBool
// already documents that "Enabled"/"Disabled" for a boolean is not
// hypothetical. Guessing from those values yields String for all three, which
// stages the operator's raw text where the firmware reads a JSON boolean, a
// JSON number, and a value from a fixed list.
func TestBiosCatalogTypesTheStagedObject(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{
		"Pcie1Enabled": "Disabled",
		"FanTrip1C":    "50",
		"FanMode":      "Automatic",
	})

	v := BiosSnapshot()
	if got := attrByName(t, v, "Pcie1Enabled").Type; got != BiosTypeBoolean {
		t.Fatalf("Pcie1Enabled type = %q, want Boolean", got)
	}
	if got := attrByName(t, v, "FanTrip1C").Type; got != BiosTypeInteger {
		t.Fatalf("FanTrip1C type = %q, want Integer", got)
	}

	res := StageBiosAttributes(
		[]string{"Pcie1Enabled", "FanTrip1C", "FanMode"},
		map[string]string{"Pcie1Enabled": "on", "FanTrip1C": "70", "FanMode": "FixedSpeed"},
	)
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}

	pending := hostBiosPending()
	if got, ok := pending["Pcie1Enabled"].(bool); !ok || !got {
		t.Errorf("staged Pcie1Enabled = %#v, want bool true — a switch submits "+
			"\"on\", and the host's Bios client reads a JSON boolean", pending["Pcie1Enabled"])
	}
	if got, ok := pending["FanTrip1C"].(int64); !ok || got != 70 {
		t.Errorf("staged FanTrip1C = %#v, want int64 70", pending["FanTrip1C"])
	}
	if got, ok := pending["FanMode"].(string); !ok || got != "FixedSpeed" {
		t.Errorf("staged FanMode = %#v, want the enum value FixedSpeed", pending["FanMode"])
	}
}

// Length bounds are the third thing a registry-less host gives the BMC no way
// to know, and the one an operator hits by pasting.
func TestBiosCatalogEnforcesStringLength(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{"EthIp4Address": "10.0.0.2"})

	res := StageBiosAttributes([]string{"EthIp4Address"},
		map[string]string{"EthIp4Address": "10.0.0.2 (was 10.0.0.1)"})

	if res.Errors["EthIp4Address"] == "" {
		t.Error("a 24-character value was accepted; the question's maxsize is 15")
	}
}

// Enforcement is the other half of describing an attribute: a value the
// firmware would reject should be rejected here, next to the field.
func TestBiosCatalogRejectsValuesTheFirmwareWouldNot(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{
		"EthIp4Mode": "Unmanaged",
		"FanTrip1C":  float64(50),
	})

	res := StageBiosAttributes(
		[]string{"EthIp4Mode", "FanTrip1C"},
		// Lowercase "dhcp" is not the vocabulary; 200C is past the 90 maximum.
		map[string]string{"EthIp4Mode": "dhcp", "FanTrip1C": "200"},
	)

	if res.Errors["EthIp4Mode"] == "" {
		t.Error(`"dhcp" was accepted; EthConfigDxe only reads Unmanaged/Dhcp/Static`)
	}
	if res.Errors["FanTrip1C"] == "" {
		t.Error("200 was accepted; the trip point maximum is 90")
	}
	if len(res.Staged) != 0 {
		t.Errorf("staged %v despite rejection", res.Staged)
	}
}

// The host owns its own vocabulary. A registry that describes an attribute
// differently — a newer firmware with a value this table has never heard of —
// must win outright.
func TestBiosRegistryBeatsTheCatalog(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, `{
  "Id": "BiosAttributeRegistry.v1_0_0",
  "RegistryEntries": {
    "Attributes": [
      {"AttributeName": "/Bios/Attributes/Pcie1MaxLinkSpeed", "DisplayName": "Link Speed",
       "Type": "Enumeration", "MenuPath": "./PCIe Configuration",
       "Value": [{"ValueName": "Gen1"}, {"ValueName": "Gen4"}]}
    ]
  }
}`)
	mergeHostBiosAttributes(map[string]any{"Pcie1MaxLinkSpeed": "Gen4"})

	a := attrByName(t, BiosSnapshot(), "Pcie1MaxLinkSpeed")

	if !a.Registered {
		t.Fatal("Registered = false for an attribute the registry described")
	}
	if got, want := optionValues(a), []string{"Gen1", "Gen4"}; !equalStrings(got, want) {
		t.Errorf("options = %v, want %v — the compiled-in list overrode the host's", got, want)
	}
	if a.MenuPath != "./PCIe Configuration" {
		t.Errorf("menu = %q, want the registry's ./PCIe Configuration", a.MenuPath)
	}
	if a.DisplayName != "Link Speed" {
		t.Errorf("display name = %q, want the registry's", a.DisplayName)
	}

	// And a value the stale table does not know must still stage.
	if res := StageBiosAttributes([]string{"Pcie1MaxLinkSpeed"},
		map[string]string{"Pcie1MaxLinkSpeed": "Gen1"}); res.Errors["Pcie1MaxLinkSpeed"] != "" {
		t.Errorf("registry-allowed value rejected: %s", res.Errors["Pcie1MaxLinkSpeed"])
	}
}

// Filling gaps is not the same as overriding. A registry that names an
// attribute but leaves its allowable values out still gets a usable dropdown.
func TestBiosCatalogFillsGapsTheRegistryLeft(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, `{
  "Id": "BiosAttributeRegistry.v1_0_0",
  "RegistryEntries": {
    "Attributes": [
      {"AttributeName": "EthIp4Mode", "DisplayName": "IPv4 Source",
       "Type": "Enumeration", "MenuPath": "./Network"}
    ]
  }
}`)
	mergeHostBiosAttributes(map[string]any{"EthIp4Mode": "Static"})

	a := attrByName(t, BiosSnapshot(), "EthIp4Mode")

	if len(a.Options) == 0 {
		t.Fatal("no options: an Enumeration with an empty value list falls back to " +
			"free text, so the registry naming it bought nothing")
	}
	if a.DisplayName != "IPv4 Source" {
		t.Errorf("display name = %q; the registry's name was replaced", a.DisplayName)
	}
	if !a.Cataloged {
		t.Error("Cataloged = false though the options came from the compiled-in table")
	}
}

// An attribute the host never reported is not invented. The BMC shows what the
// host said, the same way the NIC collection stays empty until a report lands.
func TestBiosCatalogInventsNoRows(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{"EthIp4Mode": "Dhcp"})

	v := BiosSnapshot()
	if len(v.Attributes) != 1 {
		names := make([]string, 0, len(v.Attributes))
		for _, a := range v.Attributes {
			names = append(names, a.Name)
		}
		t.Errorf("attributes = %v, want only the one the host reported", names)
	}
}

// A host reporting something outside the catalog keeps working exactly as it
// did: guessed type, leftovers bucket, no badge claiming otherwise.
func TestBiosCatalogLeavesUnknownAttributesAlone(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{"VendorSecretKnob": "whatever"})

	a := attrByName(t, BiosSnapshot(), "VendorSecretKnob")
	if a.Cataloged {
		t.Error("Cataloged = true for an attribute no table describes")
	}
	if a.MenuPath != unregisteredMenuPath {
		t.Errorf("menu = %q, want %q", a.MenuPath, unregisteredMenuPath)
	}
	if a.Type != BiosTypeString {
		t.Errorf("type = %q, want the inferred String", a.Type)
	}
}

// Every entry has to be self-consistent, because a typo here is a dropdown
// that cannot round-trip: an Enumeration with no values renders free text, and
// bounds on the wrong type are silently ignored by biosCoerce.
func TestBiosCatalogEntriesAreWellFormed(t *testing.T) {
	for name, e := range biosCatalog {
		switch e.Type {
		case BiosTypeEnumeration:
			if len(e.Options) == 0 {
				t.Errorf("%s: Enumeration with no values falls back to free text", name)
			}
		case BiosTypeInteger:
			if len(e.Options) != 0 {
				t.Errorf("%s: Integer carries options, which no control reads", name)
			}
			if e.LowerBound != nil && e.UpperBound != nil && *e.LowerBound > *e.UpperBound {
				t.Errorf("%s: lower bound %d above upper bound %d",
					name, *e.LowerBound, *e.UpperBound)
			}
		case BiosTypeString:
			if e.LowerBound != nil || e.UpperBound != nil {
				t.Errorf("%s: String carries numeric bounds, which biosCoerce ignores", name)
			}
		case BiosTypeBoolean:
			if len(e.Options) != 0 {
				t.Errorf("%s: Boolean carries options; a switch reads none", name)
			}
		default:
			t.Errorf("%s: type %q is not one an editor knows", name, e.Type)
		}
		if e.MenuPath == "" || e.MenuPath == unregisteredMenuPath {
			t.Errorf("%s: menu = %q, so a described attribute still lands in the bucket",
				name, e.MenuPath)
		}
		if e.DisplayName == "" {
			t.Errorf("%s: no DisplayName", name)
		}
		seen := map[string]bool{}
		for _, o := range e.Options {
			if o.Value == "" {
				t.Errorf("%s: option with an empty value", name)
			}
			if seen[o.Value] {
				t.Errorf("%s: duplicate option %q", name, o.Value)
			}
			seen[o.Value] = true
		}
	}
}

// The catalog is transcribed from the platform firmware's *Map.uni files, and
// the whole point is that it stays in step with them. This pins the roster so
// a formset added there without a matching entry here shows up as a failure
// rather than as one more text box on the page.
func TestBiosCatalogCoversEveryPlatformFormset(t *testing.T) {
	// rpi5-uefi-build: Platform/RaspberryPi{,/RPi5}/Drivers/*/(*Map.uni)
	want := []string{
		// EthConfigDxe
		"EthIp4Mode", "EthIp4Address", "EthIp4SubnetMask",
		"EthIp4Gateway", "EthIp4Dns1", "EthIp4Dns2",
		// FanConfigDxe
		"FanMode", "FanFixedLevel", "FanTrip1C", "FanTrip2C", "FanTrip3C", "FanTrip4C",
		// RpiPlatformDxe
		"SystemTableMode", "AcpiSdCompatMode", "AcpiSdLimitUhs",
		"AcpiPcieEcamCompatMode", "AcpiPcie32BitBarSpaceSizeMB",
		"Pcie1Enabled", "Pcie1MaxLinkSpeed",
		// BootloaderConfigDxe
		"BlBootOrder", "BlBootUart", "BlPowerOffOnHalt", "BlWakeOnGpio", "BlPsuMaxCurrent",
		// MemoryAttributeManagerDxe
		"MemoryAttributeProtocol",
		// SecureBootToggleDxe (RPI5_SECURE_BOOT=1 builds only)
		"SecureBoot",
		// RpiScmiConfigDxe
		"PowerProfile",
	}
	for _, name := range want {
		if _, ok := biosCatalog[name]; !ok {
			t.Errorf("%s is published by the platform firmware but not described here", name)
		}
	}
	if len(biosCatalog) != len(want) {
		t.Errorf("catalog holds %d entries, roster lists %d — one of the two moved without the other",
			len(biosCatalog), len(want))
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func attrByName(t *testing.T, v BiosView, name string) BiosAttribute {
	t.Helper()
	for _, a := range v.Attributes {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("%s missing from the view", name)
	return BiosAttribute{}
}

func optionValues(a BiosAttribute) []string {
	out := make([]string, 0, len(a.Options))
	for _, o := range a.Options {
		out = append(out, o.Value)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The rail is the other half of "described": a cataloged attribute has to
// produce a navigable menu entry, not just leave the leftovers bucket. The
// path is one segment even though it contains a slash — menuSegments treats
// " / " as part of an EDK2 screen name, which is why this one is worth
// pinning alongside a plain path.
func TestBiosCatalogMenusReachTheRail(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{
		"EthIp4Mode":      "Dhcp",
		"SystemTableMode": "Acpi",
		"AcpiSdLimitUhs":  true,
	})

	byPath := map[string]BiosMenu{}
	for _, mn := range BiosSnapshot().Menus {
		byPath[mn.Path] = mn
	}

	for _, tc := range []struct {
		path  string
		label string
		count int
	}{
		{"./IPv4 (BMC Managed)", "IPv4 (BMC Managed)", 1},
		{"./ACPI / Device Tree", "ACPI / Device Tree", 2},
	} {
		mn, ok := byPath[tc.path]
		if !ok {
			t.Errorf("%s missing from the rail; its attributes are unreachable", tc.path)
			continue
		}
		if mn.DisplayName != tc.label {
			t.Errorf("%s label = %q, want %q", tc.path, mn.DisplayName, tc.label)
		}
		if mn.Depth != 1 {
			t.Errorf("%s depth = %d, want 1 — the slash was read as a separator",
				tc.path, mn.Depth)
		}
		if mn.Count != tc.count {
			t.Errorf("%s holds %d attributes, want %d", tc.path, mn.Count, tc.count)
		}
	}
	if _, ok := byPath[unregisteredMenuPath]; ok {
		t.Error("the leftovers menu still exists though every attribute is described")
	}
}
