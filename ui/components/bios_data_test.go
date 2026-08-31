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
	"regexp"
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

// A cataloged attribute is described, just not by the host. It gets its own
// badge rather than the "unregistered" one — and never both, which would read
// as the row contradicting itself.
func TestBiosCatalogedAttributesAreMarkedDistinctly(t *testing.T) {
	m := sampleModel()
	m.Sections = []BiosSection{{Path: "./IPv4 (BMC Managed)", Attrs: []BiosAttr{{
		Name: "EthIp4Mode", Label: "IPv4 Policy", Control: BiosControlSelect,
		Value: "Dhcp", Cataloged: true,
		Options: []BiosOption{{Value: "Dhcp", Label: "DHCP"}},
	}}}}
	m.AttrCount = 1

	html := renderContent(t, m)
	if !strings.Contains(html, "platform default") {
		t.Errorf("no badge saying the description is the BMC's, not the host's:\n%s", html)
	}
	if strings.Contains(html, ">unregistered<") {
		t.Error("marked unregistered as well; the two badges contradict each other")
	}
}

// ── dialog layout ───────────────────────────────────────────────────────
//
// The panel is one grid: identity over the rail in the left column, search
// heading the content column, footer spanning both. Before this it was a rail
// beside a column whose own header started to the right of it, which gave the
// dialog two competing left edges and left the attribute rows stretching a
// label and its control to opposite ends of a 6xl dialog.

func renderBiosPanel(t *testing.T, m BiosModel) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return BiosPanel(m).Render(context.Background(), w)
	})
}

func TestBiosPanelIsOneTwoColumnGrid(t *testing.T) {
	html := renderBiosPanel(t, sampleModel())

	if !strings.Contains(html, "grid-cols-[14rem_minmax(0,1fr)]") {
		t.Error("the panel is not a two-column grid, so the identity block and the " +
			"rail cannot share a column edge")
	}
	// Identity must come before the rail in source order — same column, row
	// above. If it drifts after, it lands in the body row.
	identity := strings.Index(html, "BIOS Configuration")
	rail := strings.Index(html, `id="bios-rail"`)
	search := strings.Index(html, `id="bios-search"`)
	if identity < 0 || rail < 0 || search < 0 {
		t.Fatal("panel is missing the identity block, rail or search")
	}
	if !(identity < search && search < rail) {
		t.Errorf("grid cells are out of order (identity %d, search %d, rail %d): the "+
			"header row must be both cells, then the body row", identity, search, rail)
	}
	// The footer spans both columns; without this it sits under the content
	// column only and the rail runs past it.
	if !strings.Contains(html, "col-span-2") {
		t.Error("the footer does not span both grid columns")
	}
}

// The rail no longer sets its own width — the grid column does. A w-* left
// behind here fights the track and the identity block stops lining up.
func TestBiosRailDoesNotSetItsOwnWidth(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return BiosRail(sampleModel(), false).Render(context.Background(), w)
	})
	if strings.Contains(html, "w-56") {
		t.Error("the rail still carries a fixed width; the grid track owns it now")
	}
}

// Contrast: the working surface and the chrome around it must be different
// planes. --border is white/10 in the dark theme and cannot carry the
// separation on its own, which is why the old all-bg-card/60 panel read flat.
func TestBiosPanelSeparatesChromeFromContent(t *testing.T) {
	html := renderBiosPanel(t, sampleModel())

	if !strings.Contains(html, "bg-card") {
		t.Error("the content region has no surface colour of its own")
	}
	if !strings.Contains(html, "bg-background") {
		t.Error("the rail, header and footer share no chrome colour")
	}
	if strings.Contains(html, "bg-card/60") || strings.Contains(html, "bg-card/30") {
		t.Error("a translucent card wash is back; that is what made the dialog flat")
	}
}

// Stage changes lives in the footer and reaches the form by id, so the form
// keeps its own hx-post and the two cannot name different endpoints.
func TestStageChangesIsAFooterActionBoundToTheForm(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return BiosStagedBar(sampleModel(), false).Render(context.Background(), w)
	})

	if !strings.Contains(html, `form="bios-form"`) {
		t.Error("the footer's submit is not associated with the attribute form, so " +
			"it cannot submit anything from outside it")
	}
	if !strings.Contains(html, `type="submit"`) {
		t.Error("the footer's stage control is not a submit button")
	}
	if strings.Contains(html, "hx-post=\"/ui/bios/stage\"") {
		t.Error("the footer duplicates the form's verb; the form owns it")
	}
}

// The sentence moved out of the form body and onto the button as a tooltip.
func TestStageChangesExplainsItselfInATooltip(t *testing.T) {
	footer := renderToString(t, func(w *strings.Builder) error {
		return BiosStagedBar(sampleModel(), false).Render(context.Background(), w)
	})
	const sentence = "applied by the host firmware at its next boot"

	if !strings.Contains(footer, sentence) {
		t.Error("the tooltip does not carry the explanation")
	}
	if !strings.Contains(footer, "data-tui-tooltip-trigger") {
		t.Error("the stage button is not a tooltip trigger")
	}
	if form := renderContent(t, sampleModel()); strings.Contains(form, sentence) {
		t.Error("the sentence is still inline in the form as well as in the tooltip")
	}
}

// A submit that reaches a form which is not rendered does nothing and reports
// nothing. HasForm is the single answer both the content branch and the
// button's disabled state derive from.
func TestStageChangesDisabledWhenThereIsNoForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    BiosModel
		want bool
	}{
		{"nothing published", BiosModel{}, true},
		{"search with no matches", BiosModel{TotalCount: 6, Query: "zzz", AttrCount: 0}, true},
		{"a menu with rows", sampleModel(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := !tc.m.HasForm(); got != tc.want {
				t.Fatalf("HasForm disabled = %v, want %v", got, tc.want)
			}
			html := renderToString(t, func(w *strings.Builder) error {
				return BiosStagedBar(tc.m, false).Render(context.Background(), w)
			})
			// The button ships disabled exactly when no form exists. Checked
			// on the tag with its class attribute stripped: the button's
			// classes carry disabled:opacity-50 and friends, which a naive
			// substring match reads as the attribute being present.
			if got := stageButtonDisabled(t, html); got != tc.want {
				t.Errorf("rendered disabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// Rows sit two-up on a wide dialog and stack their control under their label.
// The old responsive Field put them on one line and pushed them apart.
func TestAttributeRowsUseATwoColumnGrid(t *testing.T) {
	html := renderContent(t, sampleModel())

	if !strings.Contains(html, "xl:grid-cols-2") {
		t.Error("attribute rows are not laid out two-up, so a wide dialog keeps the " +
			"dead middle space")
	}
	if strings.Contains(html, "@md/field-group:flex-row") {
		t.Error("a responsive Field is back: it puts the label and control on one " +
			"line and stretches the gap between them")
	}
	if strings.Contains(html, `Class: "w-56"`) || strings.Contains(html, "w-56") {
		t.Error("a fixed-width control is back; controls fill their grid cell now")
	}
}

// stageButtonDisabled reports whether the footer's submit carries the disabled
// attribute, ignoring the disabled:* Tailwind variants in its class list.
func stageButtonDisabled(t *testing.T, html string) bool {
	t.Helper()
	tag := regexp.MustCompile(`<button[^>]*form="bios-form"[^>]*>`).FindString(html)
	if tag == "" {
		t.Fatalf("no stage button in:\n%s", html)
	}
	return strings.Contains(regexp.MustCompile(`class="[^"]*"`).ReplaceAllString(tag, ""), "disabled")
}
