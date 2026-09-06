package components

// bios_data.go is the render model for the BIOS configuration page.
//
// It deliberately mirrors rather than reuses api/redfish's BiosAttribute.
// Components in this package import only pkg/*, never api/* — presentation is
// kept downstream of transport — and the two models answer different
// questions anyway. The Redfish-side type knows about JSON types, coercion and
// what is staged; this one knows which control to draw and what string to seed
// it with. ui/fragments_bios.go owns the translation, which is where the
// decision "an Enumeration with options becomes a select" belongs.

// BiosControl is the editor to draw for an attribute.
type BiosControl string

const (
	BiosControlSwitch   BiosControl = "switch"
	BiosControlSelect   BiosControl = "select"
	BiosControlNumber   BiosControl = "number"
	BiosControlText     BiosControl = "text"
	BiosControlPassword BiosControl = "password"
	// BiosControlReadOnly renders the value as text with no editor, for
	// attributes the registry marked ReadOnly, Immutable or GrayOut.
	BiosControlReadOnly BiosControl = "readonly"
)

// BiosOption is one choice in a select.
type BiosOption struct {
	Value string
	Label string
}

// BiosAttr is one attribute row.
type BiosAttr struct {
	Name    string
	Label   string
	Help    string
	Warning string
	Control BiosControl

	// Value seeds every control except the switch; Bool seeds the switch.
	// Both already account for staging — a staged value wins over the live
	// one, so the form shows what will be applied, not what is running.
	Value string
	Bool  bool

	// Current is the live host-reported value, shown only on a staged row so
	// the change reads as "8 (was 16)".
	Current string

	// Staged marks an attribute whose value differs from the live one.
	Staged bool

	// Unregistered marks an attribute the host reported without describing in
	// its AttributeRegistry: shown, editable, but with its type guessed from
	// the value rather than declared. There is no compiled-in per-platform
	// vocabulary to fall back on — the host's registry is the only source of
	// typed description this BMC has.
	Unregistered bool

	Options []BiosOption

	// Bounds, pre-rendered for the HTML attributes. Empty means unbounded,
	// which is why these are strings rather than pointers-to-number.
	Min       string
	Max       string
	MinLength string
	MaxLength string

	// Error is a per-attribute validation message from the last submit. The
	// row renders it inline so a rejected value is reported next to the field
	// that caused it rather than only in a toast.
	Error string
}

// HasError reports whether the last submit rejected this attribute.
func (a BiosAttr) HasError() bool { return a.Error != "" }

// ShowCurrent reports whether the "was X" hint is worth drawing: only for a
// staged attribute that actually has a live value to contrast against.
func (a BiosAttr) ShowCurrent() bool { return a.Staged && a.Current != "" }

// BiosSection is one labelled group of rows. An empty Label means the rows
// belong to the menu the page is showing and need no sub-heading of their own.
type BiosSection struct {
	Path  string
	Label string
	Attrs []BiosAttr
}

// BiosMenuItem is one entry in the page's left rail.
type BiosMenuItem struct {
	Path  string
	Label string
	// Depth indents nested menus ("./Advanced/CPU" sits under "./Advanced").
	Depth int
	// Count is how many attributes the menu holds, Staged how many of those
	// carry a pending change.
	Count  int
	Staged int
	Active bool
}

// BiosModel is everything the BIOS page renders from.
type BiosModel struct {
	BiosVersion string
	RegistryID  string
	HasRegistry bool

	Menus []BiosMenuItem

	// MenuPath / MenuLabel identify the menu being shown. Empty MenuPath with
	// a non-empty Query means search results are showing instead.
	MenuPath  string
	MenuLabel string

	// Query is the active search term; when set, Sections holds matches from
	// every menu rather than one menu's contents.
	Query string

	// Sections are the groups of rows to draw, in order. A section with no
	// Label belongs to the menu named in MenuLabel; a labelled one is a
	// sub-menu rendered inline beneath it, which is how a menu that files all
	// its attributes under children still has something to show.
	Sections []BiosSection

	// AttrCount is how many rows Sections holds in total, for the "N matching
	// attributes" line above a search result.
	AttrCount int

	// Staged is every pending attribute across all menus, for the staged bar.
	StagedCount int
	Staged      []BiosAttr

	// TotalCount is how many attributes exist in total, shown in the header so
	// an operator can tell a filtered view from the whole set.
	TotalCount int
}

// Searching reports whether the content region is showing search results.
func (m BiosModel) Searching() bool { return m.Query != "" }

// HasForm reports whether the content region is rendering an editable form.
// The footer's submit button lives outside that region and reaches it by id,
// so it needs the same answer BiosContent branches on — derived here rather
// than judged twice, or the button ends up enabled over an empty state.
func (m BiosModel) HasForm() bool {
	if m.Empty() {
		return false
	}
	return !(m.Searching() && m.AttrCount == 0)
}

// Empty reports whether there is nothing at all to configure — no registry and
// no reported attributes, which is what a host that has never booted far
// enough to publish looks like.
func (m BiosModel) Empty() bool { return m.TotalCount == 0 }
