package fragments

// fragments_bios.go serves the BIOS configuration page's fragments and owns
// the translation between api/redfish's BiosView (what an attribute *is*) and
// components.BiosModel (which control to draw for it).
//
// Every response from here is built by biosRespond, which writes the content
// region plus out-of-band copies of the menu rail and the staged-changes bar.
// See the header of ui/components/bios.templ for why all three travel
// together rather than each refetching itself on a shared event: the rail's
// active item depends on the request that caused the update, so a rail that
// refetches itself has no way to know what it should highlight.

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func biosFragmentRoutes(g *gin.RouterGroup, _ *deps.Deps) {
	b := g.Group("/bios")

	b.GET("", getBiosPanel)
	b.GET("/menu", getBiosMenu)
	b.GET("/search", getBiosSearch)
	b.POST("/stage", postBiosStage)
	b.POST("/discard", postBiosDiscard)
}

// getBiosPanel serves the dialog's whole inner layout. Requested once each
// time the dialog is opened, so an operator always sees live host state rather
// than whatever was true when the page was served.
func getBiosPanel(c *gin.Context) {
	renderFragment(c, components.BiosPanel(biosModel("", "", nil)))
}

// getBiosMenu switches the content region to one menu's attributes.
func getBiosMenu(c *gin.Context) {
	biosRespond(c, biosModel(c.Query("path"), "", nil))
}

// getBiosSearch filters across every menu. An empty query falls back to the
// first menu rather than rendering an empty result set, so clearing the search
// box returns to browsing instead of to a blank pane.
func getBiosSearch(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		biosRespond(c, biosModel("", "", nil))
		return
	}
	biosRespond(c, biosModel("", q, nil))
}

// postBiosStage merges the submitted attributes into the pending set.
//
// The form posts a "__attr" roster naming every attribute it owned alongside
// the values themselves. Both are needed: an unchecked switch submits no value
// at all, so without the roster "turned off" is indistinguishable from "not on
// this form" and could never be staged.
func postBiosStage(c *gin.Context) {
	names := c.PostFormArray("__attr")
	vals := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := c.GetPostForm(n); ok {
			vals[n] = v
		}
	}

	res := redfish.StageBiosAttributes(names, vals)
	biosStageToast(c, res)

	// Re-render whatever region the form was submitted from, carrying any
	// per-attribute errors back to the fields that caused them.
	biosRespond(c, biosModel(c.PostForm("menu"), strings.TrimSpace(c.PostForm("q")), res.Errors))
}

// biosStageToast reports what the submission did. Errors win over successes:
// a partially-applied submit must not read as a clean save.
func biosStageToast(c *gin.Context, res redfish.BiosStageResult) {
	switch {
	case len(res.Errors) > 0:
		hxToast(c, "error", biosCountLabel(len(res.Errors), "attribute", "attributes")+" rejected",
			"Corrected values are shown inline. "+biosStagedSummary(res))
	case len(res.Staged) > 0 && len(res.Reverted) > 0:
		hxToast(c, "success", "Changes staged", biosStagedSummary(res))
	case len(res.Staged) > 0:
		hxToast(c, "success", biosCountLabel(len(res.Staged), "change", "changes")+" staged",
			"The host firmware applies them at its next boot.")
	case len(res.Reverted) > 0:
		hxToast(c, "success", biosCountLabel(len(res.Reverted), "change", "changes")+" reverted",
			"Those attributes now match what the host is running.")
	default:
		hxToast(c, "info", "No changes", "Every submitted value already matched what is staged.")
	}
}

func biosStagedSummary(res redfish.BiosStageResult) string {
	parts := make([]string, 0, 2)
	if n := len(res.Staged); n > 0 {
		parts = append(parts, biosCountLabel(n, "change", "changes")+" staged")
	}
	if n := len(res.Reverted); n > 0 {
		parts = append(parts, biosCountLabel(n, "change", "changes")+" reverted")
	}
	if len(parts) == 0 {
		return "Nothing was staged."
	}
	return strings.Join(parts, ", ") + "."
}

func biosCountLabel(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

// postBiosDiscard drops one staged attribute (?attr=…) or the whole set.
func postBiosDiscard(c *gin.Context) {
	if name := c.Query("attr"); name != "" {
		if redfish.DiscardBiosAttribute(name) {
			hxToast(c, "success", "Change discarded", name+" is back to what the host is running.")
		} else {
			hxToast(c, "info", "Nothing to discard", name+" had no staged change.")
		}
	} else {
		redfish.DiscardBiosPending()
		hxToast(c, "success", "Staged changes discarded", "The host keeps running its current settings.")
	}
	biosRespond(c, biosModel(c.Query("menu"), strings.TrimSpace(c.Query("q")), nil))
}

// biosRespond writes the content region plus the out-of-band rail and staged
// bar, so one response leaves every part of the page agreeing.
func biosRespond(c *gin.Context, m components.BiosModel) {
	renderFragment(c, components.BiosContentResponse(m))
}

// ── model building ──────────────────────────────────────────────────────

// biosModel builds the render model. menuPath selects the menu to show (empty
// picks the first); query, when set, replaces the menu listing with matches
// from every menu; errs carries per-attribute validation messages from a
// submit so they render beside the fields that produced them.
func biosModel(menuPath, query string, errs map[string]string) components.BiosModel {
	view := redfish.BiosSnapshot()

	m := components.BiosModel{
		BiosVersion: view.BiosVersion,
		RegistryID:  view.RegistryID,
		HasRegistry: view.HasRegistry,
		Query:       query,
		StagedCount: view.PendingCount,
		TotalCount:  len(view.Attributes),
	}

	// The rail always shows the whole menu tree, search or not.
	menuLabels := make(map[string]string, len(view.Menus))
	for _, mn := range view.Menus {
		menuLabels[mn.Path] = biosMenuLabel(mn)
	}
	if menuPath == "" && query == "" && len(view.Menus) > 0 {
		menuPath = view.Menus[0].Path
	}
	for _, mn := range view.Menus {
		m.Menus = append(m.Menus, components.BiosMenuItem{
			Path:   mn.Path,
			Label:  biosMenuLabel(mn),
			Depth:  mn.Depth,
			Count:  mn.Count,
			Staged: mn.PendingCount,
			Active: query == "" && mn.Path == menuPath,
		})
	}

	var sections []redfish.BiosSection
	if query != "" {
		sections = view.SearchSections(query)
	} else {
		m.MenuPath = menuPath
		m.MenuLabel = menuLabels[menuPath]
		sections = view.Sections(menuPath)
	}
	for _, s := range sections {
		sec := components.BiosSection{Path: s.Path, Label: s.Label}
		for _, a := range s.Attrs {
			sec.Attrs = append(sec.Attrs, biosAttr(a, errs[a.Name]))
		}
		m.AttrCount += len(sec.Attrs)
		m.Sections = append(m.Sections, sec)
	}

	for _, a := range view.Pending() {
		m.Staged = append(m.Staged, biosAttr(a, ""))
	}
	return m
}

// biosMenuLabel titles a rail entry, falling back to the path's last segment
// when the registry named a menu but gave it no DisplayName.
func biosMenuLabel(mn redfish.BiosMenu) string {
	if mn.DisplayName != "" {
		return mn.DisplayName
	}
	return mn.Path
}

// biosAttr maps one domain attribute onto its render model — the point where
// "what this attribute is" becomes "which control to draw".
func biosAttr(a redfish.BiosAttribute, errMsg string) components.BiosAttr {
	out := components.BiosAttr{
		Name:    a.Name,
		Label:   a.Label(),
		Help:    a.HelpText,
		Warning: a.WarningText,
		Control: biosControlFor(a),
		Value:   a.ValueString(),
		Bool:    a.BoolValue(),
		Current: a.CurrentString(),
		Staged:  a.HasPending,
		// Cataloged wins: an attribute the BMC's platform table describes is
		// not an undescribed one, and rendering both badges would have the
		// row contradict itself. A registry entry the table merely topped up
		// is Registered and Cataloged at once — still worth marking, because
		// some of what the operator sees is not what the host asserted.
		Unregistered: !a.Registered && !a.Cataloged,
		Cataloged:    a.Cataloged,
		Min:          biosBound(a.LowerBound),
		Max:          biosBound(a.UpperBound),
		MinLength:    biosBound(a.MinLength),
		MaxLength:    biosBound(a.MaxLength),
		Error:        errMsg,
	}
	for _, o := range a.Options {
		out.Options = append(out.Options, components.BiosOption{Value: o.Value, Label: o.Label})
	}
	// A select whose current value is not among the registry's allowed values
	// still has to render that value, or the control would silently show
	// something the host is not running. Surface it as an extra option marked
	// as such rather than dropping it.
	if out.Control == components.BiosControlSelect && out.Value != "" && !biosHasOption(out.Options, out.Value) {
		out.Options = append(out.Options, components.BiosOption{
			Value: out.Value,
			Label: out.Value + " (current, not in registry)",
		})
	}
	return out
}

// biosControlFor picks the editor. Read-only wins over everything: the
// registry marking an attribute ReadOnly/Immutable/GrayOut means an operator
// must not be offered a way to change it.
func biosControlFor(a redfish.BiosAttribute) components.BiosControl {
	if a.ReadOnly {
		return components.BiosControlReadOnly
	}
	switch a.Type {
	case redfish.BiosTypeBoolean:
		return components.BiosControlSwitch
	case redfish.BiosTypeInteger:
		return components.BiosControlNumber
	case redfish.BiosTypePassword:
		return components.BiosControlPassword
	case redfish.BiosTypeEnumeration:
		// An Enumeration the registry gave no values for cannot be a select —
		// it would render an empty dropdown that can only ever clear the
		// value. Fall back to free text so the attribute stays editable.
		if len(a.Options) == 0 {
			return components.BiosControlText
		}
		return components.BiosControlSelect
	default:
		return components.BiosControlText
	}
}

func biosHasOption(opts []components.BiosOption, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

// biosBound renders an optional numeric bound for an HTML attribute; an
// absent bound is the empty string, which the component omits.
func biosBound(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}
