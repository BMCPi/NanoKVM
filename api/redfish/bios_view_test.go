package redfish

// bios_view_test.go covers the join (registry + live + pending → view) and the
// write path (form strings → typed staged values). The type coercion is the
// part that matters most: the host firmware reads the staged object and
// expects the JSON types it declared, so staging "true" where it wants true,
// or "4096" where it wants 4096, is a silently broken setting rather than a
// visible error.

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// seedRegistry installs a registry document built from the given raw JSON.
func seedRegistry(t *testing.T, doc string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("bad test registry: %v", err)
	}
	setHostBiosRegistry(m)
}

// testRegistry is a small but representative EDK2-shaped registry: one menu
// tree, one attribute of each type, a read-only entry and a hidden one.
const testRegistry = `{
  "Id": "BiosAttributeRegistry.v1_0_0",
  "RegistryVersion": "1.2.3",
  "RegistryEntries": {
    "Menus": [
      {"MenuName": "Advanced", "DisplayName": "Advanced", "MenuPath": "./Advanced", "DisplayOrder": 2},
      {"MenuName": "Main", "DisplayName": "Main", "MenuPath": "./Main", "DisplayOrder": 1},
      {"MenuName": "CPU", "DisplayName": "Processor", "MenuPath": "./Advanced/CPU", "DisplayOrder": 1},
      {"MenuName": "Secret", "DisplayName": "Secret", "MenuPath": "./Secret", "Hidden": true}
    ],
    "Attributes": [
      {"AttributeName": "ProcHyperThreading", "DisplayName": "Hyper-Threading",
       "HelpText": "Enable simultaneous multithreading.", "Type": "Enumeration",
       "MenuPath": "./Advanced/CPU", "DisplayOrder": 2,
       "Value": [{"ValueName": "Enabled", "ValueDisplayName": "Enabled"},
                 {"ValueName": "Disabled", "ValueDisplayName": "Disabled"}]},
      {"AttributeName": "ProcCoreCount", "DisplayName": "Active Cores", "Type": "Integer",
       "MenuPath": "./Advanced/CPU", "DisplayOrder": 1, "LowerBound": 0, "UpperBound": 16},
      {"AttributeName": "BootTimeout", "DisplayName": "Boot Timeout", "Type": "Integer",
       "MenuPath": "./Main", "LowerBound": 0, "UpperBound": 60, "DefaultValue": 5},
      {"AttributeName": "QuietBoot", "DisplayName": "Quiet Boot", "Type": "Boolean",
       "MenuPath": "./Main"},
      {"AttributeName": "AssetTag", "DisplayName": "Asset Tag", "Type": "String",
       "MenuPath": "./Main", "MinLength": 2, "MaxLength": 8},
      {"AttributeName": "AdminPassword", "DisplayName": "Admin Password", "Type": "Password",
       "MenuPath": "./Main"},
      {"AttributeName": "CpuSignature", "DisplayName": "CPU Signature", "Type": "String",
       "MenuPath": "./Advanced/CPU", "ReadOnly": true},
      {"AttributeName": "InternalKnob", "DisplayName": "Internal", "Type": "String",
       "MenuPath": "./Advanced", "Hidden": true}
    ]
  }
}`

func TestBiosSnapshotJoinsRegistryLiveAndPending(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{
		"ProcHyperThreading": "Enabled",
		"ProcCoreCount":      float64(16),
		"QuietBoot":          true,
		"CpuSignature":       "0x000806F8",
	})
	setHostBiosPending(map[string]any{"ProcCoreCount": int64(8)})

	v := BiosSnapshot()

	if !v.HasRegistry {
		t.Error("HasRegistry = false, want true")
	}
	if v.RegistryVersion != "1.2.3" {
		t.Errorf("RegistryVersion = %q, want 1.2.3", v.RegistryVersion)
	}
	if v.PendingCount != 1 {
		t.Errorf("PendingCount = %d, want 1", v.PendingCount)
	}

	byName := map[string]BiosAttribute{}
	for _, a := range v.Attributes {
		byName[a.Name] = a
	}

	if _, ok := byName["InternalKnob"]; ok {
		t.Error("a Hidden attribute must not appear in the view")
	}
	if !byName["CpuSignature"].ReadOnly {
		t.Error("CpuSignature should be read-only")
	}

	ht := byName["ProcHyperThreading"]
	if ht.Label() != "Hyper-Threading" {
		t.Errorf("Label = %q, want Hyper-Threading", ht.Label())
	}
	if len(ht.Options) != 2 {
		t.Errorf("Options = %d, want 2", len(ht.Options))
	}
	if ht.ValueString() != "Enabled" {
		t.Errorf("ValueString = %q, want Enabled", ht.ValueString())
	}

	// The staged value wins for the editor; the live value is still reachable
	// for the "was X" hint.
	cores := byName["ProcCoreCount"]
	if !cores.HasPending {
		t.Fatal("ProcCoreCount should carry a staged change")
	}
	if got := cores.ValueString(); got != "8" {
		t.Errorf("ValueString = %q, want 8 (the staged value)", got)
	}
	if got := cores.CurrentString(); got != "16" {
		t.Errorf("CurrentString = %q, want 16 (the live value)", got)
	}

	// A registry DefaultValue stands in until the host reports a live one.
	if got := byName["BootTimeout"].ValueString(); got != "5" {
		t.Errorf("BootTimeout ValueString = %q, want the registry default 5", got)
	}
}

func TestBiosSnapshotMenusAreOrderedAndCounted(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	setHostBiosPending(map[string]any{"ProcCoreCount": int64(8)})

	v := BiosSnapshot()

	var paths []string
	for _, m := range v.Menus {
		paths = append(paths, m.Path)
	}
	// Main (DisplayOrder 1) before Advanced (2); CPU nested under Advanced.
	want := []string{"./Main", "./Advanced", "./Advanced/CPU"}
	if len(paths) != len(want) {
		t.Fatalf("menus = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("menus = %v, want %v", paths, want)
		}
	}

	for _, m := range v.Menus {
		switch m.Path {
		case "./Advanced/CPU":
			if m.DisplayName != "Processor" {
				t.Errorf("CPU DisplayName = %q, want Processor", m.DisplayName)
			}
			if m.Depth != 2 {
				t.Errorf("CPU Depth = %d, want 2", m.Depth)
			}
			if m.Count != 3 {
				t.Errorf("CPU Count = %d, want 3", m.Count)
			}
			if m.PendingCount != 1 {
				t.Errorf("CPU PendingCount = %d, want 1", m.PendingCount)
			}
		case "./Secret":
			t.Error("a Hidden menu must not appear")
		}
	}
}

// An attribute the host reported without describing must still be editable —
// otherwise a host that never publishes a registry gets an empty screen.
func TestBiosSnapshotShowsUnregisteredAttributes(t *testing.T) {
	resetHostState(t)
	mergeHostBiosAttributes(map[string]any{
		"MysteryFlag":   true,
		"MysteryNumber": float64(42),
		"MysteryText":   "hello",
	})

	v := BiosSnapshot()
	if v.HasRegistry {
		t.Error("HasRegistry = true with no registry published")
	}
	if len(v.Attributes) != 3 {
		t.Fatalf("attributes = %d, want 3", len(v.Attributes))
	}

	byName := map[string]BiosAttribute{}
	for _, a := range v.Attributes {
		byName[a.Name] = a
		if a.Registered {
			t.Errorf("%s marked registered with no registry", a.Name)
		}
		if a.MenuPath != unregisteredMenuPath {
			t.Errorf("%s menu = %q, want %q", a.Name, a.MenuPath, unregisteredMenuPath)
		}
	}
	if got := byName["MysteryFlag"].Type; got != BiosTypeBoolean {
		t.Errorf("MysteryFlag type = %q, want Boolean", got)
	}
	if got := byName["MysteryNumber"].Type; got != BiosTypeInteger {
		t.Errorf("MysteryNumber type = %q, want Integer", got)
	}
	if got := byName["MysteryText"].Type; got != BiosTypeString {
		t.Errorf("MysteryText type = %q, want String", got)
	}
}

// The core of the write path: a form submits strings, and the staged object
// must carry the JSON types the host declared.
func TestStageBiosAttributesCoercesTypes(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{
		"ProcCoreCount":      float64(16),
		"QuietBoot":          false,
		"AssetTag":           "old",
		"ProcHyperThreading": "Enabled",
	})

	res := StageBiosAttributes(
		[]string{"ProcCoreCount", "QuietBoot", "AssetTag", "ProcHyperThreading"},
		map[string]string{
			"ProcCoreCount":      "8",
			"QuietBoot":          "true",
			"AssetTag":           "rack42",
			"ProcHyperThreading": "Disabled",
		})

	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Staged) != 4 {
		t.Fatalf("staged = %v, want all four", res.Staged)
	}

	pending := hostBiosPending()
	if got, ok := pending["ProcCoreCount"].(int64); !ok || got != 8 {
		t.Errorf("ProcCoreCount staged as %#v, want int64(8)", pending["ProcCoreCount"])
	}
	if got, ok := pending["QuietBoot"].(bool); !ok || !got {
		t.Errorf("QuietBoot staged as %#v, want bool(true)", pending["QuietBoot"])
	}
	if got, ok := pending["AssetTag"].(string); !ok || got != "rack42" {
		t.Errorf("AssetTag staged as %#v, want string", pending["AssetTag"])
	}
	if got := pending["ProcHyperThreading"]; got != "Disabled" {
		t.Errorf("ProcHyperThreading staged as %#v", got)
	}
}

// Staging is a diff: a value equal to the live one clears the stage instead of
// staging a no-op, so the settings object reads as "what will change".
func TestStageBiosAttributesRevertsWhenValueMatchesLive(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{"ProcCoreCount": float64(16)})
	setHostBiosPending(map[string]any{"ProcCoreCount": int64(8)})

	res := StageBiosAttributes([]string{"ProcCoreCount"}, map[string]string{"ProcCoreCount": "16"})

	if len(res.Reverted) != 1 || res.Reverted[0] != "ProcCoreCount" {
		t.Errorf("Reverted = %v, want [ProcCoreCount]", res.Reverted)
	}
	if len(res.Staged) != 0 {
		t.Errorf("Staged = %v, want empty", res.Staged)
	}
	if _, ok := hostBiosPending()["ProcCoreCount"]; ok {
		t.Error("ProcCoreCount should have been dropped from the staged set")
	}
}

// The live value arrives as float64 out of JSON while a staged one is int64.
// Without normalised comparison a staged integer would never look equal to its
// own live value and could never clear.
func TestStageBiosAttributesComparesAcrossJSONNumericTypes(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{"ProcCoreCount": float64(8)})

	res := StageBiosAttributes([]string{"ProcCoreCount"}, map[string]string{"ProcCoreCount": "8"})
	if len(res.Staged) != 0 {
		t.Errorf("Staged = %v; 8 == 8.0 must not stage a no-op", res.Staged)
	}
	if len(hostBiosPending()) != 0 {
		t.Errorf("pending = %v, want empty", hostBiosPending())
	}
}

// A UI that shows one menu at a time submits only that menu. Attributes it did
// not own must keep their staged values.
func TestStageBiosAttributesLeavesUnsubmittedStagingAlone(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{"ProcCoreCount": float64(16), "AssetTag": "old"})
	setHostBiosPending(map[string]any{"AssetTag": "staged-elsewhere"})

	StageBiosAttributes([]string{"ProcCoreCount"}, map[string]string{"ProcCoreCount": "8"})

	pending := hostBiosPending()
	if got := pending["AssetTag"]; got != "staged-elsewhere" {
		t.Errorf("AssetTag = %#v; another menu's staged value was clobbered", got)
	}
	if got, ok := pending["ProcCoreCount"].(int64); !ok || got != 8 {
		t.Errorf("ProcCoreCount = %#v, want int64(8)", pending["ProcCoreCount"])
	}
}

// An unchecked switch submits nothing at all. The roster of submitted names is
// what distinguishes "turned off" from "not on this form".
func TestStageBiosAttributesTreatsAbsentBooleanAsFalse(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{"QuietBoot": true})

	// Named in the roster, absent from the values: the switch was turned off.
	res := StageBiosAttributes([]string{"QuietBoot"}, map[string]string{})

	if len(res.Staged) != 1 {
		t.Fatalf("Staged = %v, want QuietBoot staged false", res.Staged)
	}
	if got, ok := hostBiosPending()["QuietBoot"].(bool); !ok || got {
		t.Errorf("QuietBoot staged as %#v, want bool(false)", hostBiosPending()["QuietBoot"])
	}
}

func TestStageBiosAttributesValidates(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)

	for _, tc := range []struct {
		name  string
		attr  string
		value string
	}{
		{"integer below LowerBound", "ProcCoreCount", "-1"},
		{"integer above UpperBound", "ProcCoreCount", "99"},
		{"integer not a number", "ProcCoreCount", "eight"},
		{"enumeration outside Value[]", "ProcHyperThreading", "Sometimes"},
		{"string under MinLength", "AssetTag", "x"},
		{"string over MaxLength", "AssetTag", "far-too-long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := StageBiosAttributes([]string{tc.attr}, map[string]string{tc.attr: tc.value})
			if _, ok := res.Errors[tc.attr]; !ok {
				t.Errorf("no error for %s=%q; errors=%v", tc.attr, tc.value, res.Errors)
			}
			if _, staged := hostBiosPending()[tc.attr]; staged {
				t.Errorf("%s was staged despite being invalid", tc.attr)
			}
		})
	}
}

func TestStageBiosAttributesRefusesReadOnlyAndUnknown(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{"CpuSignature": "0x1"})

	res := StageBiosAttributes(
		[]string{"CpuSignature", "NoSuchAttribute"},
		map[string]string{"CpuSignature": "0xDEAD", "NoSuchAttribute": "x"})

	if _, staged := hostBiosPending()["CpuSignature"]; staged {
		t.Error("a read-only attribute must never be staged")
	}
	if _, ok := res.Errors["NoSuchAttribute"]; !ok {
		t.Error("want an error for an unknown attribute")
	}
}

func TestDiscardBiosPending(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	setHostBiosPending(map[string]any{"ProcCoreCount": int64(8), "AssetTag": "x"})

	if !DiscardBiosAttribute("AssetTag") {
		t.Error("DiscardBiosAttribute(AssetTag) = false, want true")
	}
	if _, ok := hostBiosPending()["AssetTag"]; ok {
		t.Error("AssetTag still staged")
	}
	if DiscardBiosAttribute("AssetTag") {
		t.Error("discarding a non-staged attribute should report false")
	}

	DiscardBiosPending()
	if got := hostBiosPending(); len(got) != 0 {
		t.Errorf("pending = %v, want empty", got)
	}
}

// A malformed registry must degrade to "no vocabulary", never take the page
// down — it is whatever the host PUT.
func TestBiosSnapshotSurvivesMalformedRegistry(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, `{"RegistryEntries": {"Attributes": ["not-an-object", {"NoName": true}],
	                                        "Menus": [42, {"MenuPath": ""}]}}`)
	mergeHostBiosAttributes(map[string]any{"Live": "value"})

	v := BiosSnapshot()
	if len(v.Attributes) != 1 || v.Attributes[0].Name != "Live" {
		t.Errorf("attributes = %+v, want just the live one", v.Attributes)
	}
}

func TestBiosViewSearch(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	v := BiosSnapshot()

	if got := v.Search(""); got != nil {
		t.Errorf("Search(\"\") = %d results, want nil", len(got))
	}
	// Matches HelpText, not just the name.
	if got := v.Search("multithreading"); len(got) != 1 || got[0].Name != "ProcHyperThreading" {
		t.Errorf("Search(multithreading) = %+v", got)
	}
	// Case-insensitive over the raw attribute name.
	if got := v.Search("proccore"); len(got) != 1 || got[0].Name != "ProcCoreCount" {
		t.Errorf("Search(proccore) = %+v", got)
	}
}

// A password control cannot be seeded with the current value, so every submit
// of a form containing one carries it back empty. Treating that as "set to the
// empty string" would stage a blank administrator password whenever an
// operator saved anything else on the same menu.
func TestStageBiosAttributesIgnoresEmptyPassword(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, testRegistry)
	mergeHostBiosAttributes(map[string]any{"QuietBoot": false})

	res := StageBiosAttributes(
		[]string{"AdminPassword", "QuietBoot"},
		map[string]string{"AdminPassword": "", "QuietBoot": "true"})

	if _, staged := hostBiosPending()["AdminPassword"]; staged {
		t.Errorf("an empty password field must not stage anything, got %#v",
			hostBiosPending()["AdminPassword"])
	}
	if len(res.Staged) != 1 || res.Staged[0] != "QuietBoot" {
		t.Errorf("Staged = %v, want just QuietBoot", res.Staged)
	}

	// A non-empty one still stages normally.
	res = StageBiosAttributes([]string{"AdminPassword"}, map[string]string{"AdminPassword": "s3cret"})
	if got := hostBiosPending()["AdminPassword"]; got != "s3cret" {
		t.Errorf("AdminPassword = %#v, want the submitted value", got)
	}
	if len(res.Staged) != 1 {
		t.Errorf("Staged = %v, want the password staged", res.Staged)
	}
}

// edk2Registry is the shape a Raspberry Pi UEFI build actually publishes, and
// every awkward part of it is load-bearing for the tests below: AttributeName
// arrives as a JSON pointer rather than the key the Attributes object uses,
// every MenuPath hangs off a "/Root" container that holds nothing, and one
// form set is named "ACPI / Device Tree" — a title with a literal slash in it.
// There is no Menus[] array at all, so the rail has to be inferred.
const edk2Registry = `{
  "Id": "BiosAttributeRegistry",
  "RegistryVersion": "1.0.0",
  "RegistryEntries": {
    "Attributes": [
      {"AttributeName": "/Bios/Attributes/SystemTableMode", "DisplayName": "System Table Mode",
       "Type": "Enumeration", "MenuPath": "/Root/Raspberry Pi Configuration/ACPI / Device Tree",
       "CurrentValue": "DeviceTree", "DefaultValue": "DeviceTree"},
      {"AttributeName": "/Bios/Attributes/AcpiSdLimitUhs", "DisplayName": "Limit UHS-I Modes",
       "Type": "Boolean", "MenuPath": "/Root/Raspberry Pi Configuration/ACPI / Device Tree",
       "DefaultValue": true},
      {"AttributeName": "/Bios/Attributes/Pcie1MaxLinkSpeed", "DisplayName": "Link Speed",
       "Type": "Enumeration", "MenuPath": "/Root/Raspberry Pi Configuration/PCI Express",
       "DefaultValue": "Gen2"},
      {"AttributeName": "/Bios/Attributes/SecureBoot", "DisplayName": "Attempt Secure Boot",
       "Type": "Boolean", "MenuPath": "/Root/Secure Boot", "DefaultValue": false}
    ]
  }
}`

// The registry names attributes by JSON pointer while the host reports values
// under the bare key. Joining on the raw string matches nothing, and the
// failure is silent: every registered attribute falls back to its default and
// every reported value reappears in the Unregistered bucket, doubling the
// attribute count.
func TestBiosSnapshotJoinsPointerStyleAttributeNames(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, edk2Registry)
	mergeHostBiosAttributes(map[string]any{
		"SystemTableMode":   "Acpi",
		"AcpiSdLimitUhs":    false,
		"Pcie1MaxLinkSpeed": "Gen3",
		"SecureBoot":        true,
	})

	v := BiosSnapshot()
	if len(v.Attributes) != 4 {
		t.Fatalf("attributes = %d, want 4 — a duplicated set means the join failed", len(v.Attributes))
	}
	byName := map[string]BiosAttribute{}
	for _, a := range v.Attributes {
		byName[a.Name] = a
		if !a.Registered {
			t.Errorf("%s is unregistered — the registry key never matched the reported one", a.Name)
		}
		if a.MenuPath == unregisteredMenuPath {
			t.Errorf("%s landed in the Unregistered bucket", a.Name)
		}
	}
	// The live value must win over the registry's DefaultValue.
	if got := byName["SystemTableMode"].Current; got != "Acpi" {
		t.Errorf("SystemTableMode current = %#v, want the reported \"Acpi\"", got)
	}
	if got, ok := byName["AcpiSdLimitUhs"].Current.(bool); !ok || got {
		t.Errorf("AcpiSdLimitUhs current = %#v, want the reported false",
			byName["AcpiSdLimitUhs"].Current)
	}
	if got := byName["SystemTableMode"].DisplayName; got != "System Table Mode" {
		t.Errorf("DisplayName = %q, want the registry's", got)
	}
}

// Staging has to write the key the firmware reads, not the pointer the
// registry described it with.
func TestStageBiosAttributesUsesTheBareAttributeKey(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, edk2Registry)
	mergeHostBiosAttributes(map[string]any{"SystemTableMode": "DeviceTree"})

	res := StageBiosAttributes([]string{"SystemTableMode"}, map[string]string{"SystemTableMode": "Acpi"})
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", res.Errors)
	}
	pending := hostBiosPending()
	if got, ok := pending["SystemTableMode"]; !ok || got != "Acpi" {
		t.Errorf("pending = %#v, want SystemTableMode staged under its bare key", pending)
	}
	if _, ok := pending["/Bios/Attributes/SystemTableMode"]; ok {
		t.Error("staged under the JSON pointer, which the host firmware never reads")
	}
}

// "/Root" is a container EDK2 puts every form set under. Keeping it adds a
// dead top-level rail entry that holds nothing — and since the rail opens on
// its first entry, that is the pane an operator lands on.
func TestBiosMenusDropTheRootContainer(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, edk2Registry)

	v := BiosSnapshot()
	for _, mn := range v.Menus {
		if mn.Path == "./Root" || strings.HasPrefix(mn.Path, "./Root/") {
			t.Errorf("menu %q still carries the Root container", mn.Path)
		}
		if mn.Depth < 1 {
			t.Errorf("menu %q has depth %d", mn.Path, mn.Depth)
		}
	}
	if len(v.Menus) == 0 {
		t.Fatal("no menus built")
	}
	// Whatever the rail opens on must have something to show.
	if got := v.Sections(v.Menus[0].Path); len(got) == 0 {
		t.Errorf("the first rail entry %q renders nothing", v.Menus[0].Path)
	}
	// A menu genuinely named RootComplex is not the container.
	if got := normalizeMenuPath("/RootComplex/Ports"); got != "./RootComplex/Ports" {
		t.Errorf("normalizeMenuPath(/RootComplex/Ports) = %q", got)
	}
}

// A "/" with a space beside it is part of a form set's title, not a path
// separator. Splitting on it invents a phantom "ACPI" menu holding nothing
// with a " Device Tree" child under it.
func TestBiosMenuPathKeepsLiteralSlashesInNames(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, edk2Registry)

	v := BiosSnapshot()
	paths := map[string]BiosMenu{}
	for _, mn := range v.Menus {
		paths[mn.Path] = mn
	}
	const want = "./Raspberry Pi Configuration/ACPI / Device Tree"
	mn, ok := paths[want]
	if !ok {
		t.Fatalf("menus = %v, want one at %q", keysOf(paths), want)
	}
	if mn.Depth != 2 {
		t.Errorf("depth = %d, want 2 — the title's slash was treated as a separator", mn.Depth)
	}
	if mn.DisplayName != "ACPI / Device Tree" {
		t.Errorf("DisplayName = %q, want the full title", mn.DisplayName)
	}
	if mn.Count != 2 {
		t.Errorf("Count = %d, want 2", mn.Count)
	}
	for _, bad := range []string{"./Raspberry Pi Configuration/ACPI ", "./Raspberry Pi Configuration/ACPI / Device Tree/ Device Tree"} {
		if _, ok := paths[bad]; ok {
			t.Errorf("phantom menu %q was created", bad)
		}
	}
}

// A menu that files every attribute under a child must still render them,
// grouped under a heading naming the child.
func TestBiosSectionsIncludeDescendantMenus(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, edk2Registry)

	v := BiosSnapshot()
	secs := v.Sections("./Raspberry Pi Configuration")
	if len(secs) != 2 {
		t.Fatalf("sections = %d, want 2 (one per child menu)", len(secs))
	}
	if secs[0].Label != "ACPI / Device Tree" || len(secs[0].Attrs) != 2 {
		t.Errorf("section 0 = %q with %d attrs", secs[0].Label, len(secs[0].Attrs))
	}
	if secs[1].Label != "PCI Express" || len(secs[1].Attrs) != 1 {
		t.Errorf("section 1 = %q with %d attrs", secs[1].Label, len(secs[1].Attrs))
	}

	// A leaf menu's own rows carry no sub-heading — the page already names it.
	leaf := v.Sections("./Secure Boot")
	if len(leaf) != 1 || leaf[0].Label != "" || len(leaf[0].Attrs) != 1 {
		t.Errorf("leaf sections = %+v, want one unlabelled section of 1", leaf)
	}
}

// Search spans menus, so each match has to say which one it came from.
func TestBiosSearchSectionsGroupByMenu(t *testing.T) {
	resetHostState(t)
	seedRegistry(t, edk2Registry)

	secs := BiosSnapshot().SearchSections("s")
	if len(secs) < 2 {
		t.Fatalf("sections = %d, want matches grouped across several menus", len(secs))
	}
	for _, s := range secs {
		if s.Label == "" {
			t.Errorf("section %q has no label; a search result must name its menu", s.Path)
		}
		if len(s.Attrs) == 0 {
			t.Errorf("section %q is empty", s.Path)
		}
	}
	// A nested menu is named by its whole path below the root.
	for _, s := range secs {
		if s.Path == "./Raspberry Pi Configuration/ACPI / Device Tree" &&
			s.Label != "Raspberry Pi Configuration › ACPI / Device Tree" {
			t.Errorf("nested label = %q, want the full path below the root", s.Label)
		}
	}
}

func keysOf(m map[string]BiosMenu) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
