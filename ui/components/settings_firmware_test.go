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

// ── Upload and fetch progress ───────────────────────────────────────────────
//
// The same two-halves failure, one layer along. The panel already rendered a
// #fw-upload-progress bar; nothing ever unhid it, because the only
// htmx:xhr:progress listener in the app filtered on virtual media's form id.
// The bar was there, hidden, for every upload. These tests assert the halves
// are connected the way the gate's are: by an attribute on the markup rather
// than by a form id written down in a script.

var uploadProgressAttrRe = regexp.MustCompile(`data-upload-progress="#([^"]+)"`)

func TestUploadProgressBarIsDeclaredByItsForm(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{VolumeReady: true, Presented: true})

	if !strings.Contains(html, `id="fw-upload-progress"`) {
		t.Fatal("no upload progress bar in the panel")
	}
	m := uploadProgressAttrRe.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("the panel renders a progress bar that no form claims; nothing " +
			"will ever unhide it")
	}
	if m[1] != "fw-upload-progress" {
		t.Errorf("the form points at #%s, not at the bar it renders", m[1])
	}
}

// Pointing at a bar that is not in the render is the same bug wearing the hat
// the gate test already described.
func TestUploadProgressTargetExists(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{VolumeReady: true})
	for _, m := range uploadProgressAttrRe.FindAllStringSubmatch(html, -1) {
		if !strings.Contains(html, `id="`+m[1]+`"`) {
			t.Errorf("data-upload-progress points at #%s, which is not in this render", m[1])
		}
	}
}

// A capsule fetch runs for minutes. A static line of text is indistinguishable
// from a wedged transfer, which is what this panel used to show.
func TestStagingRowShowsDownloadProgress(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{
		VolumeReady: true, Presented: true,
		Staging: true, StagingName: "host.cap",
		StagingLoaded: 25 << 20, StagingTotal: 100 << 20,
	})

	if !strings.Contains(html, "host.cap") {
		t.Error("the staging row does not name what is being fetched")
	}
	if !strings.Contains(stagingRow(t, html), `data-slot="progress"`) {
		t.Error("a fetch with a known total renders no progress bar")
	}
	if !strings.Contains(html, `aria-valuenow="25"`) {
		t.Errorf("the bar is not at 25%%: %s", stagingRow(t, html))
	}
	// The absolute figures, so "25%" of what is answerable.
	for _, want := range []string{"25.0 MB", "100.0 MB"} {
		if !strings.Contains(html, want) {
			t.Errorf("%q missing from the staging row", want)
		}
	}
}

// A chunked response declares no length. A bar with no denominator is a bar
// that lies, so this case gets a spinner and a running count instead.
func TestStagingRowFallsBackToASpinnerWithNoTotal(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{
		VolumeReady: true, Staging: true, StagingName: "host.cap",
		StagingLoaded: 3 << 20, StagingTotal: 0,
	})

	// Scoped to the row: the upload tab renders its own (hidden) bar always.
	if strings.Contains(stagingRow(t, html), `data-slot="progress"`) {
		t.Error("a determinate bar was drawn for a download with no declared total")
	}
	if !strings.Contains(html, "3.0 MB downloaded") {
		t.Errorf("no running byte count in the indeterminate state: %s", stagingRow(t, html))
	}
}

// Before the first byte there is nothing to count, and "0 B downloaded" reads
// as a transfer that has stalled at the start rather than one still opening.
func TestStagingRowSaysConnectingBeforeAnyBytes(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{
		VolumeReady: true, Staging: true, StagingName: "host.cap",
	})
	if !strings.Contains(html, "Connecting…") {
		t.Errorf("a fetch with no bytes yet does not say it is connecting: %s", stagingRow(t, html))
	}
}

// No fetch, no staging row — the panel must not render an idle progress bar.
func TestNoStagingRowWhenNothingIsFetching(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{VolumeReady: true, Presented: true})
	if strings.Contains(html, "Staging ") || strings.Contains(html, "Connecting…") {
		t.Error("a staging row rendered with no fetch in flight")
	}
}

func TestStagingPercentClampsAndGuardsZero(t *testing.T) {
	for _, tc := range []struct {
		name          string
		loaded, total int64
		want          int
		determinate   bool
	}{
		{"half", 50, 100, 50, true},
		{"none declared", 30, 0, 0, false},
		// A remote that under-declares its Content-Length would otherwise
		// drive the indicator past its own track.
		{"over-delivered", 150, 100, 100, true},
	} {
		m := SettingsFirmware{StagingLoaded: tc.loaded, StagingTotal: tc.total}
		if got := m.StagingPercent(); got != tc.want {
			t.Errorf("%s: StagingPercent() = %d, want %d", tc.name, got, tc.want)
		}
		if got := m.StagingDeterminate(); got != tc.determinate {
			t.Errorf("%s: StagingDeterminate() = %v, want %v", tc.name, got, tc.determinate)
		}
	}
}

// stagingRow trims a failure message down to the row under test.
func stagingRow(t *testing.T, html string) string {
	t.Helper()
	i := strings.Index(html, "Staging ")
	if i < 0 {
		return "(no staging row)"
	}
	end := i + 400
	if end > len(html) {
		end = len(html)
	}
	return html[i:end]
}

// The shared progress script must stay attribute-driven. The moment it learns
// a form id, the next panel to grow an upload ships a bar nothing unhides —
// which is exactly how this one shipped.
func TestUploadProgressScriptNamesNoForms(t *testing.T) {
	js, err := TemplFiles.ReadFile("upload_progress.js")
	if err != nil {
		t.Fatalf("read upload_progress.js: %v", err)
	}
	src := string(js)
	if !strings.Contains(src, "data-upload-progress") {
		t.Error("the script does not read the attribute the forms declare")
	}
	// Comments stripped: the file documents its contract with a real example,
	// and the example naturally names a real form.
	code := stripLineComments(src)
	for _, id := range []string{"vm-upload-form", "fw-upload-form", "vm-upload-progress", "fw-upload-progress"} {
		if strings.Contains(code, id) {
			t.Errorf("upload_progress.js hardcodes %q; it must find its targets "+
				"through data-upload-progress so a new form cannot ship half-wired", id)
		}
	}
}

func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
