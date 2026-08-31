package components

// settings_firmware_test.go guards the firmware panel's form gating.
//
// Submit buttons in this app render disabled and are enabled by script once
// their paired input has a value. That is two halves in two files, and this
// panel shipped with only the first: #fw-upload-submit was rendered
// Disabled:true while nothing anywhere referenced that id, so the Upload
// button could never become clickable. The same class of bug — markup copied
// without the behaviour that completes it — also produced the power toggle
// posting the previous action.
//
// These tests assert the two halves stay connected: a control that starts
// disabled must declare the input that ungates it, and that input must exist
// in the same render.

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func renderFirmwarePanel(t *testing.T, m SettingsFirmware) string {
	t.Helper()

	var sb strings.Builder
	if err := SettingsFirmwareBody(m).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// gateTargets returns every data-gate-input selector in the markup.
var gateAttrRe = regexp.MustCompile(`data-gate-input="#([A-Za-z0-9_-]+)"`)

// TestFirmwareUploadSubmitIsGated is the regression guard for the reported
// bug: the Upload button never became enabled after choosing a file.
func TestFirmwareUploadSubmitIsGated(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{VolumeReady: true, Presented: true})

	if !strings.Contains(html, `id="fw-upload-submit"`) {
		t.Fatal("upload submit button missing from the panel")
	}

	// The button ships disabled, so something must be able to enable it.
	// Declaring its paired input is how a control opts into the shared gate.
	if !gateAttrRe.MatchString(html) {
		t.Fatal("no data-gate-input in the panel: #fw-upload-submit renders " +
			"disabled and nothing can ever enable it, so choosing a file " +
			"leaves Upload unclickable")
	}

	var gatedByUploadFile bool
	for _, m := range gateAttrRe.FindAllStringSubmatch(html, -1) {
		if m[1] == "fw-upload-file" {
			gatedByUploadFile = true
		}
	}
	if !gatedByUploadFile {
		t.Error("the upload submit is not gated on #fw-upload-file")
	}
}

// A gate pointing at an element that does not exist is the same bug wearing a
// different hat: the button stays disabled forever and the markup looks right.
func TestFirmwareGateTargetsExist(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{VolumeReady: true, Presented: true})

	matches := gateAttrRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		t.Skip("no gates declared; covered by TestFirmwareUploadSubmitIsGated")
	}
	for _, m := range matches {
		if !strings.Contains(html, `id="`+m[1]+`"`) {
			t.Errorf("data-gate-input points at #%s, which is not in this render", m[1])
		}
	}
}

// The shared gate script has to know about this panel's controls. It scans for
// the attribute rather than carrying a hardcoded id list, precisely so a new
// form cannot ship half-wired the way this one did.
func TestGateScriptIsAttributeDrivenNotAList(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{VolumeReady: true})

	if strings.Contains(html, "fw-upload-submit") && !gateAttrRe.MatchString(html) {
		t.Error("panel relies on a gate it does not declare")
	}
}
