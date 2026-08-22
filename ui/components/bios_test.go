package components

// bios_test.go covers the rendering contract the BIOS page's htmx layering
// depends on. Three things here are load-bearing and none of them are visible
// from a screenshot:
//
//   - every editable row emits a hidden "__attr" input naming itself, which is
//     the only way the handler can tell a switch that was turned off from an
//     attribute that was not on the submitted form at all;
//   - a read-only row emits no editor AND no roster entry, so it can never be
//     submitted even by a hand-crafted POST that echoes the form;
//   - a fragment response carries hx-swap-oob copies of the rail and staged
//     bar, which is what keeps the three regions from disagreeing.

import (
	"context"
	"strings"
	"testing"
)

func renderToString(t *testing.T, render func(w *strings.Builder) error) string {
	t.Helper()
	var sb strings.Builder
	if err := render(&sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// sampleModel is a small model exercising every control kind at once.
func sampleModel() BiosModel {
	return BiosModel{
		BiosVersion: "EDK2-1.2",
		HasRegistry: true,
		TotalCount:  6,
		MenuPath:    "./Advanced/CPU",
		MenuLabel:   "Processor",
		Menus: []BiosMenuItem{
			{Path: "./Main", Label: "Main", Depth: 1, Count: 3},
			{Path: "./Advanced", Label: "Advanced", Depth: 1, Count: 0},
			{Path: "./Advanced/CPU", Label: "Processor", Depth: 2, Count: 3, Staged: 1, Active: true},
		},
		AttrCount: 6,
		Sections: []BiosSection{{
			Path: "./Advanced/CPU",
			Attrs: []BiosAttr{
				{
					Name: "ProcHyperThreading", Label: "Hyper-Threading", Help: "Simultaneous multithreading.",
					Control: BiosControlSelect, Value: "Enabled",
					Options: []BiosOption{{Value: "Enabled", Label: "Enabled"}, {Value: "Disabled", Label: "Disabled"}},
				},
				{
					Name: "ProcCoreCount", Label: "Active Cores", Control: BiosControlNumber,
					Value: "8", Current: "16", Staged: true, Min: "0", Max: "16",
				},
				{Name: "QuietBoot", Label: "Quiet Boot", Control: BiosControlSwitch, Bool: true},
				{Name: "AssetTag", Label: "Asset Tag", Control: BiosControlText, Value: "rack42", MaxLength: "8"},
				{Name: "AdminPassword", Label: "Admin Password", Control: BiosControlPassword},
			},
		}, {
			Path:  "./Advanced/CPU/Identification",
			Label: "Identification",
			Attrs: []BiosAttr{
				{Name: "CpuSignature", Label: "CPU Signature", Control: BiosControlReadOnly, Value: "0x000806F8"},
			},
		}},
		StagedCount: 1,
		Staged: []BiosAttr{
			{Name: "ProcCoreCount", Label: "Active Cores", Control: BiosControlNumber, Value: "8", Current: "16", Staged: true},
		},
	}
}

func renderContent(t *testing.T, m BiosModel) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return BiosContent(m).Render(context.Background(), w)
	})
}

func renderResponse(t *testing.T, m BiosModel) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return BiosContentResponse(m).Render(context.Background(), w)
	})
}

// Every editable attribute must name itself in the roster. Without it the
// handler cannot distinguish "this switch was turned off" (submits nothing)
// from "this attribute was not on the form", and an off switch never stages.
func TestBiosFormEmitsRosterForEditableAttributesOnly(t *testing.T) {
	html := renderContent(t, sampleModel())

	for _, name := range []string{
		"ProcHyperThreading", "ProcCoreCount", "QuietBoot", "AssetTag", "AdminPassword",
	} {
		want := `<input type="hidden" name="__attr" value="` + name + `"`
		if !strings.Contains(html, want) {
			t.Errorf("editable attribute %s has no roster entry", name)
		}
	}

	// The read-only attribute must not be in the roster, so it cannot be
	// staged even by a POST that echoes the form back with it added.
	if strings.Contains(html, `name="__attr" value="CpuSignature"`) {
		t.Error("a read-only attribute must not appear in the roster")
	}
}

func TestBiosFormRendersControlPerType(t *testing.T) {
	html := renderContent(t, sampleModel())

	for _, tc := range []struct {
		what string
		want string
	}{
		{"number input", `type="number"`},
		{"number lower bound", `min="0"`},
		{"number upper bound", `max="16"`},
		{"text maxlength", `maxlength="8"`},
		{"password input", `type="password"`},
		{"select option", "Disabled"},
		{"read-only value", "0x000806F8"},
		{"staged badge", "staged"},
		{"current-value hint", "16"},
	} {
		if !strings.Contains(html, tc.want) {
			t.Errorf("%s missing: no %q in rendered form", tc.what, tc.want)
		}
	}

	// A read-only attribute renders its value as text, never as an input.
	if strings.Contains(html, `name="CpuSignature"`) {
		t.Error("a read-only attribute must not render a named form control")
	}
}

// The layering contract: one response updates content, rail and staged bar.
func TestBiosContentResponseCarriesOutOfBandRegions(t *testing.T) {
	html := renderResponse(t, sampleModel())

	if n := strings.Count(html, `hx-swap-oob="true"`); n != 2 {
		t.Errorf("hx-swap-oob elements = %d, want 2 (rail + staged bar)", n)
	}
	for _, id := range []string{`id="bios-rail"`, `id="bios-staged"`} {
		if !strings.Contains(html, id) {
			t.Errorf("response is missing %s, so that region would go stale", id)
		}
	}
	// The content itself must NOT be out-of-band: it is the hx-target swap.
	form := html[:strings.Index(html, `hx-swap-oob`)]
	if strings.Contains(form, `hx-swap-oob`) {
		t.Error("the content region must not be marked out-of-band")
	}
}

func TestBiosRailMarksActiveAndStagedMenus(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return BiosRail(sampleModel(), false).Render(context.Background(), w)
	})

	if !strings.Contains(html, `data-active="true"`) {
		t.Error("the active menu is not marked")
	}
	if strings.Count(html, `data-active="true"`) != 1 {
		t.Error("exactly one menu should be active")
	}
	// The nested menu links to itself with its path escaped.
	if !strings.Contains(html, "path=.%2FAdvanced%2FCPU") {
		t.Errorf("nested menu path is not URL-escaped in its hx-get:\n%s", html)
	}
	// Depth 2 indents; depth 1 does not.
	if !strings.Contains(html, "padding-left:1.25rem") {
		t.Error("a depth-2 menu should be indented")
	}
}

func TestBiosStagedBarShowsChangesAndDiscardControls(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return BiosStagedBar(sampleModel(), false).Render(context.Background(), w)
	})

	if !strings.Contains(html, "1 pending change") {
		t.Errorf("staged count not rendered:\n%s", html)
	}
	if !strings.Contains(html, "/ui/bios/discard?attr=ProcCoreCount") {
		t.Error("per-attribute discard control missing")
	}
	if !strings.Contains(html, "/ui/bios/discard?menu=") {
		t.Error("discard-all control missing")
	}

	// Both discard controls carry where the operator is, or acting on a chip
	// would silently navigate them back to the first menu.
	if !strings.Contains(html, "attr=ProcCoreCount&amp;menu=.%2FAdvanced%2FCPU") {
		t.Errorf("per-chip discard does not preserve the current menu:\n%s", html)
	}
	if !strings.Contains(html, "/ui/bios/discard?menu=.%2FAdvanced%2FCPU") {
		t.Error("discard-all does not preserve the current menu")
	}
}

// While searching, a discard must return to the same results rather than to a
// menu the operator was not looking at.
func TestBiosStagedBarPreservesSearchOnDiscard(t *testing.T) {
	m := sampleModel()
	m.MenuPath, m.Query = "", "core"

	html := renderToString(t, func(w *strings.Builder) error {
		return BiosStagedBar(m, false).Render(context.Background(), w)
	})

	if !strings.Contains(html, "/ui/bios/discard?q=core") {
		t.Errorf("discard-all drops the active search:\n%s", html)
	}
	if !strings.Contains(html, "attr=ProcCoreCount&amp;q=core") {
		t.Error("per-chip discard drops the active search")
	}
	if strings.Contains(html, "menu=") {
		t.Error("a searching model should not pin a menu on its discard controls")
	}
}

func TestBiosStagedBarEmptyState(t *testing.T) {
	m := sampleModel()
	m.StagedCount, m.Staged = 0, nil

	html := renderToString(t, func(w *strings.Builder) error {
		return BiosStagedBar(m, false).Render(context.Background(), w)
	})

	if !strings.Contains(html, "No pending changes") {
		t.Error("empty staged bar should say so")
	}
	if strings.Contains(html, "Discard all") {
		t.Error("nothing staged: the discard control should not be offered")
	}
}

// A staged password must never be echoed back into the page.
func TestBiosStagedChipHidesPasswordValues(t *testing.T) {
	m := sampleModel()
	m.StagedCount = 1
	m.Staged = []BiosAttr{{
		Name: "AdminPassword", Label: "Admin Password",
		Control: BiosControlPassword, Value: "hunter2", Staged: true,
	}}

	html := renderToString(t, func(w *strings.Builder) error {
		return BiosStagedBar(m, false).Render(context.Background(), w)
	})

	if strings.Contains(html, "hunter2") {
		t.Error("a staged password value was echoed into the staged bar")
	}
	if !strings.Contains(html, "set") {
		t.Error("a staged password should still show that one is set")
	}
}

func TestBiosFormRendersInlineErrors(t *testing.T) {
	m := sampleModel()
	m.Sections[0].Attrs[1].Error = "must be at most 16"

	html := renderContent(t, m)
	if !strings.Contains(html, "must be at most 16") {
		t.Error("a rejected attribute's message should render beside its field")
	}
	if !strings.Contains(html, `role="alert"`) {
		t.Error("the inline error should be announced to assistive tech")
	}
}

func TestBiosEmptyStateWhenNothingReported(t *testing.T) {
	html := renderContent(t, BiosModel{})

	if !strings.Contains(html, "No BIOS attributes yet") {
		t.Errorf("empty model should render the explanatory empty state:\n%s", html)
	}
	if strings.Contains(html, "Stage changes") {
		t.Error("nothing to configure: no submit control should be offered")
	}
}

func TestBiosSearchResultsAndNoMatches(t *testing.T) {
	m := sampleModel()
	m.Query = "core"
	m.MenuPath = ""
	m.Sections = []BiosSection{{Path: "./Advanced/CPU", Label: "Advanced \u203a Processor", Attrs: []BiosAttr{m.Sections[0].Attrs[1]}}}
	m.AttrCount = 1

	html := renderContent(t, m)
	if !strings.Contains(html, "across all menus") {
		t.Error("search results should say they span every menu")
	}
	// The query is echoed back into the form so a submit from search results
	// re-renders the same result set rather than jumping to a menu.
	if !strings.Contains(html, `name="q" value="core"`) {
		t.Error("the active query should be echoed into the form")
	}

	m.Sections, m.AttrCount = nil, 0
	if html := renderContent(t, m); !strings.Contains(html, "No attribute matches") {
		t.Errorf("a search with no matches should say so:\n%s", html)
	}
}

func TestBiosUnregisteredAttributesAreMarked(t *testing.T) {
	m := sampleModel()
	m.Sections = []BiosSection{{Path: "./Unregistered", Attrs: []BiosAttr{{
		Name: "MysteryKnob", Label: "MysteryKnob",
		Control: BiosControlText, Value: "x", Unregistered: true,
	}}}}
	m.AttrCount = 1

	html := renderContent(t, m)
	if !strings.Contains(html, "unregistered") {
		t.Error("an attribute with no registry entry should be marked as such")
	}
	if !strings.Contains(html, `name="__attr" value="MysteryKnob"`) {
		t.Error("an unregistered attribute should still be editable")
	}
}

// The dialog must not fetch anything until it is opened, and must re-fetch on
// every open — host state changes underneath it, so a body rendered once at
// page load would go stale the moment the host reported anything.
func TestBiosDialogLoadsLazilyOnTrigger(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return BiosDialog().Render(context.Background(), w)
	})

	if !strings.Contains(html, `hx-get="/ui/bios"`) {
		t.Error("dialog body does not load the panel")
	}
	if !strings.Contains(html, `hx-trigger="click from:#bios-open-trigger"`) {
		t.Errorf("dialog body should load on the trigger, not on page load:\n%s", html)
	}
	// Nothing may be bound to "load": that would cost a request on every page
	// view whether or not anyone opens the dialog.
	if strings.Contains(html, `hx-trigger="load`) {
		t.Error("dialog body must not fetch on page load")
	}
	if !strings.Contains(html, `id="bios-dialog"`) {
		t.Error("dialog is missing the id its trigger addresses")
	}
}

func TestBiosDialogTriggerAddressesTheDialog(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return BiosDialogTrigger().Render(context.Background(), w)
	})

	if !strings.Contains(html, `id="bios-open-trigger"`) {
		t.Error("trigger is missing the id the dialog body listens to")
	}
	if !strings.Contains(html, "bios-dialog") {
		t.Errorf("trigger does not address the dialog:\n%s", html)
	}
}

// The panel is one fragment carrying all three regions, so opening the dialog
// paints a complete surface in a single request.
func TestBiosPanelCarriesEveryRegion(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return BiosPanel(sampleModel()).Render(context.Background(), w)
	})

	for _, id := range []string{`id="bios-rail"`, `id="bios-content"`, `id="bios-staged"`} {
		if !strings.Contains(html, id) {
			t.Errorf("panel is missing %s", id)
		}
	}
	// Nothing in the initial panel is out-of-band: it is swapped wholesale.
	if strings.Contains(html, "hx-swap-oob") {
		t.Error("the initial panel must not mark regions out-of-band")
	}
	if !strings.Contains(html, "BIOS Configuration") {
		t.Error("panel heading missing")
	}
}

// The registry's MenuPath is what groups rows and its DisplayName is what
// titles the group, so a menu that files its attributes under children renders
// them inline under sub-headings rather than showing a blank pane.
func TestBiosFormRendersSubSectionHeadings(t *testing.T) {
	html := renderContent(t, sampleModel())

	// The menu's own name sits above the sub-sections, not as one of them.
	if !strings.Contains(html, `<h3 class="mb-2 text-sm font-semibold">Processor</h3>`) {
		t.Error("the active menu should be named above its sections")
	}
	if !strings.Contains(html, "Identification") {
		t.Error("a descendant menu should render as a labelled sub-section")
	}
	// Both sections' rows are in the one form, so a submit carries all of them.
	for _, name := range []string{"ProcHyperThreading", "CpuSignature"} {
		if !strings.Contains(html, `data-bios-attr="`+name+`"`) {
			t.Errorf("%s is missing; every section's rows belong to the same form", name)
		}
	}
	if strings.Count(html, `id="bios-form"`) != 1 {
		t.Error("sections must share one form, or a submit would only carry one of them")
	}
}
