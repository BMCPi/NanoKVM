package components

import (
	"strings"
)

// OverviewServer is the Server Information card body: the host-reported
// identity of the managed server (with SMBIOS enrichment where captured).
// Zero value renders placeholders; the /ui/overview/server fragment supplies
// the populated model.
type OverviewServer struct {
	Board           string
	Vendor          string
	CPU             string
	Memory          string
	Serial          string
	Revision        string
	InventorySource string
}

// OverviewUpdateCheck is one "current version + latest release" pair. Checked
// is false until a fragment has actually asked upstream, so first paint can
// render the version row without implying "no update available".
type OverviewUpdateCheck struct {
	Current         string
	Latest          string
	UpdateAvailable bool
	Checked         bool
}

// OverviewHostFirmware is the Host Firmware card body: what the managed
// host's firmware last reported over the Redfish host interface, plus the
// staged boot override. Zero value renders placeholders for first paint;
// the /ui/overview/firmware fragment supplies the populated model.
type OverviewHostFirmware struct {
	BiosVersion  string
	BootOverride string
	BootProgress string
}

// OverviewBootOverride is the Boot Override card body: the override staged
// on the BMC for the host firmware to pick up at its next boot. Target and
// Enabled use the Redfish Boot vocabulary (BootSourceOverrideTarget /
// BootSourceOverrideEnabled).
type OverviewBootOverride struct {
	Target  string
	Enabled string
}

// Active reports whether an override is actually staged (a real target with
// Once/Continuous persistence).
func (m OverviewBootOverride) Active() bool {
	return m.Target != "" && m.Target != "None" &&
		m.Enabled != "" && m.Enabled != "Disabled"
}

// SelectValue is the value the target select should show: the staged target
// while one is active, None otherwise (also the zero model's first paint).
func (m OverviewBootOverride) SelectValue() string {
	if m.Active() {
		return m.Target
	}
	return "None"
}

// ModeValue is the persistence the power menu's Apply button submits:
// whatever is staged, so reopening the menu offers to re-apply what is
// already in effect rather than silently resetting to Once.
func (m OverviewBootOverride) ModeValue() string {
	if m.Enabled == "Continuous" {
		return "continuous"
	}
	return "once"
}

// ModeLabel is ModeValue for display, on the split button's face.
func (m OverviewBootOverride) ModeLabel() string {
	if m.ModeValue() == "continuous" {
		return "Continuous"
	}
	return "Once"
}

// BootOverrideLabel renders the staged state for the read-only Host Firmware
// row: "Pxe · once", "Hdd · continuous", or "none" when nothing is staged.
func (m OverviewBootOverride) BootOverrideLabel() string {
	if !m.Active() {
		return "none"
	}
	return m.Target + " · " + strings.ToLower(m.Enabled)
}

// versionDisplay normalizes a version to v-prefixed form; "" and "dev" pass
// through so placeholders and dev builds stay honest.
func versionDisplay(v string) string {
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "dev" {
		return v
	}
	return "v" + v
}
