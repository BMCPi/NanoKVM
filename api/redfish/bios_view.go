package redfish

// bios_view.go turns the three raw BIOS documents this BMC stores into
// something a UI can render, and turns a submitted form back into a staged
// Attributes object.
//
// The three sources (see bios.go):
//
//   - the AttributeRegistry the host published — the *vocabulary*: which
//     attribute keys exist, what type each is, what values it accepts, what
//     menu it belongs under, and what to call it in a UI;
//   - Bios.Attributes — the live values the host last reported;
//   - Bios/Settings.Attributes — the operator-staged pending values.
//
// The BMC deliberately validates nothing against a key catalog on the Redfish
// surface: the host owns the vocabulary. That stays true here. The registry is
// used to *present* attributes and to coerce submitted form strings back to
// the JSON type the host declared — not to reject keys. An attribute the host
// reported without a registry entry is still shown and still editable, with
// its type inferred from the value it currently holds, because a host that has
// not published a registry would otherwise present an empty screen.
//
// Staging is a diff, not a snapshot. The pending set is exactly the attributes
// whose staged value differs from the live one: submitting a value equal to
// the current one removes it from the stage rather than staging a no-op. That
// is what makes the settings object readable as "what will change at the next
// boot", and it is why the write path merges rather than replaces — a UI that
// shows one menu at a time must not wipe the changes staged under another.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// BIOS attribute types, as spelled in an AttributeRegistry (DSP0268).
const (
	BiosTypeEnumeration = "Enumeration"
	BiosTypeString      = "String"
	BiosTypeInteger     = "Integer"
	BiosTypeBoolean     = "Boolean"
	BiosTypePassword    = "Password"
)

// unregisteredMenuPath collects attributes the host reported but did not
// describe in its registry. A separate menu rather than a silent merge into
// the real ones: they carry no display name, help text or validation, and an
// operator editing them should be able to see that is what they are.
const unregisteredMenuPath = "./Unregistered"

// BiosOption is one allowable value of an Enumeration attribute.
type BiosOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// BiosAttribute is one row of the BIOS configuration UI: what the attribute
// is, what it currently holds, and what (if anything) is staged for it.
type BiosAttribute struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	HelpText    string `json:"helpText"`
	WarningText string `json:"warningText"`
	Type        string `json:"type"`
	MenuPath    string `json:"menuPath"`
	Order       int    `json:"order"`

	// ReadOnly covers the registry's ReadOnly, Immutable and GrayOut flags:
	// all three mean "an operator must not be offered an editor for this".
	ReadOnly bool `json:"readOnly"`

	// Registered is false for an attribute the host reported but did not
	// describe, whose Type was inferred from its current value.
	Registered bool `json:"registered"`

	// Current is the live host-reported value; nil when the host has reported
	// the key only through the registry and never a value for it.
	Current any `json:"current"`

	// Pending is the staged value and HasPending says whether one exists —
	// separate because a legitimately staged value can be a zero value.
	Pending    any  `json:"pending"`
	HasPending bool `json:"hasPending"`

	Options []BiosOption `json:"options,omitempty"`

	// Bounds from the registry; nil when it declared none.
	LowerBound *int64 `json:"lowerBound,omitempty"`
	UpperBound *int64 `json:"upperBound,omitempty"`
	MinLength  *int64 `json:"minLength,omitempty"`
	MaxLength  *int64 `json:"maxLength,omitempty"`
}

// Value is what an editor should be seeded with: the staged value when one
// exists, otherwise the live one.
func (a BiosAttribute) Value() any {
	if a.HasPending {
		return a.Pending
	}
	return a.Current
}

// ValueString renders Value for an HTML control.
func (a BiosAttribute) ValueString() string { return biosValueString(a.Value()) }

// CurrentString renders the live value, for the "was X" hint on a staged row.
func (a BiosAttribute) CurrentString() string { return biosValueString(a.Current) }

// BoolValue interprets Value for a switch control.
func (a BiosAttribute) BoolValue() bool {
	b, _ := biosToBool(a.Value())
	return b
}

// Label is the name to show: the registry's DisplayName when it gave one,
// else the raw attribute key.
func (a BiosAttribute) Label() string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.Name
}

// BiosMenu is one node of the registry's menu tree.
type BiosMenu struct {
	Path        string `json:"path"`
	DisplayName string `json:"displayName"`
	Order       int    `json:"order"`
	// Depth is how deeply nested the menu is; "./Advanced" is 1,
	// "./Advanced/CPU" is 2. The rail indents by it.
	Depth int `json:"depth"`
	// Count is how many attributes this menu holds directly.
	Count int `json:"count"`
	// PendingCount is how many of those have a staged change, so the rail can
	// mark which menus an operator has touched.
	PendingCount int `json:"pendingCount"`
}

// BiosView is the whole BIOS configuration surface in one value.
type BiosView struct {
	BiosVersion     string          `json:"biosVersion"`
	RegistryID      string          `json:"registryId"`
	RegistryVersion string          `json:"registryVersion"`
	HasRegistry     bool            `json:"hasRegistry"`
	Menus           []BiosMenu      `json:"menus"`
	Attributes      []BiosAttribute `json:"attributes"`
	PendingCount    int             `json:"pendingCount"`
}

// Menu returns the attributes under one menu path, in display order.
func (v BiosView) Menu(path string) []BiosAttribute {
	out := make([]BiosAttribute, 0, 16)
	for _, a := range v.Attributes {
		if a.MenuPath == path {
			out = append(out, a)
		}
	}
	return out
}

// Pending returns every attribute carrying a staged change, in display order.
func (v BiosView) Pending() []BiosAttribute {
	out := make([]BiosAttribute, 0, 8)
	for _, a := range v.Attributes {
		if a.HasPending {
			out = append(out, a)
		}
	}
	return out
}

// Search returns attributes whose name, display name or help text contains q
// (case-insensitive). An empty q returns nothing rather than everything: the
// caller renders a menu in that case, and returning the full set here would
// make an empty search box look like a filter that matched all attributes.
func (v BiosView) Search(q string) []BiosAttribute {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	out := make([]BiosAttribute, 0, 16)
	for _, a := range v.Attributes {
		if strings.Contains(strings.ToLower(a.Name), q) ||
			strings.Contains(strings.ToLower(a.DisplayName), q) ||
			strings.Contains(strings.ToLower(a.HelpText), q) {
			out = append(out, a)
		}
	}
	return out
}

// BiosSnapshot joins the registry, the live attributes and the staged set into
// one renderable view.
func BiosSnapshot() BiosView {
	reported, _ := HostReported()
	live := hostBiosAttributes()
	pending := hostBiosPending()
	reg := hostBiosRegistry()

	v := BiosView{BiosVersion: reported.BiosVersion, HasRegistry: reg != nil}
	if reg != nil {
		if id, ok := reg["Id"].(string); ok {
			v.RegistryID = id
		}
		if rv, ok := reg["RegistryVersion"].(string); ok {
			v.RegistryVersion = rv
		}
	}

	entries, menus := parseBiosRegistry(reg)

	// Registry-described attributes first, in the order the registry gave.
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Name == "" || e.Hidden {
			continue
		}
		seen[e.Name] = true
		a := BiosAttribute{
			Name:        e.Name,
			DisplayName: e.DisplayName,
			HelpText:    e.HelpText,
			WarningText: e.WarningText,
			Type:        e.Type,
			MenuPath:    e.MenuPath,
			Order:       e.Order,
			ReadOnly:    e.ReadOnly,
			Registered:  true,
			Options:     e.Options,
			LowerBound:  e.LowerBound,
			UpperBound:  e.UpperBound,
			MinLength:   e.MinLength,
			MaxLength:   e.MaxLength,
		}
		if cur, ok := live[e.Name]; ok {
			a.Current = cur
		} else if e.HasDefault {
			// No live report yet: the registry's DefaultValue is the closest
			// thing to a current value, and leaves the control seeded with
			// something the firmware would actually accept.
			a.Current = e.DefaultValue
		}
		if p, ok := pending[e.Name]; ok {
			a.Pending, a.HasPending = p, true
		}
		v.Attributes = append(v.Attributes, a)
	}

	// Then anything the host reported (or staged) without describing.
	extra := make([]string, 0, 8)
	for name := range live {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	for name := range pending {
		if !seen[name] && !containsString(extra, name) {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		cur := live[name]
		a := BiosAttribute{
			Name:       name,
			Type:       biosInferType(cur),
			MenuPath:   unregisteredMenuPath,
			Registered: false,
			Current:    cur,
		}
		if p, ok := pending[name]; ok {
			a.Pending, a.HasPending = p, true
			if cur == nil {
				a.Type = biosInferType(p)
			}
		}
		v.Attributes = append(v.Attributes, a)
	}

	sortBiosAttributes(v.Attributes)
	v.Menus = buildBiosMenus(menus, v.Attributes)
	for _, a := range v.Attributes {
		if a.HasPending {
			v.PendingCount++
		}
	}
	return v
}

// ── registry parsing ────────────────────────────────────────────────────

// biosRegEntry is one parsed RegistryEntries.Attributes[] element.
type biosRegEntry struct {
	Name         string
	DisplayName  string
	HelpText     string
	WarningText  string
	Type         string
	MenuPath     string
	Order        int
	ReadOnly     bool
	Hidden       bool
	Options      []BiosOption
	DefaultValue any
	HasDefault   bool
	LowerBound   *int64
	UpperBound   *int64
	MinLength    *int64
	MaxLength    *int64
}

// biosRegMenu is one parsed RegistryEntries.Menus[] element.
type biosRegMenu struct {
	Path        string
	DisplayName string
	Order       int
	Hidden      bool
}

// parseBiosRegistry pulls the attribute and menu entries out of a registry
// document. Everything is defensive: the document is whatever the host PUT,
// and a malformed corner of it must degrade to "that entry is unusable"
// rather than to an empty BIOS screen.
func parseBiosRegistry(reg map[string]any) ([]biosRegEntry, []biosRegMenu) {
	if reg == nil {
		return nil, nil
	}
	root, _ := reg["RegistryEntries"].(map[string]any)
	if root == nil {
		return nil, nil
	}

	rawAttrs, _ := root["Attributes"].([]any)
	entries := make([]biosRegEntry, 0, len(rawAttrs))
	for _, raw := range rawAttrs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		e := biosRegEntry{
			Name:        stringField(m, "AttributeName"),
			DisplayName: stringField(m, "DisplayName"),
			HelpText:    stringField(m, "HelpText"),
			WarningText: stringField(m, "WarningText"),
			Type:        stringField(m, "Type"),
			MenuPath:    normalizeMenuPath(stringField(m, "MenuPath")),
			Order:       int(intField(m, "DisplayOrder")),
			// ReadOnly, Immutable and GrayOut all mean the same thing to an
			// editor: do not offer one.
			ReadOnly:   boolField(m, "ReadOnly") || boolField(m, "Immutable") || boolField(m, "GrayOut"),
			Hidden:     boolField(m, "Hidden"),
			LowerBound: optIntField(m, "LowerBound"),
			UpperBound: optIntField(m, "UpperBound"),
			MinLength:  optIntField(m, "MinLength"),
			MaxLength:  optIntField(m, "MaxLength"),
		}
		if e.Type == "" {
			e.Type = BiosTypeString
		}
		if dv, ok := m["DefaultValue"]; ok && dv != nil {
			e.DefaultValue, e.HasDefault = dv, true
		}
		if vals, ok := m["Value"].([]any); ok {
			for _, rv := range vals {
				vm, ok := rv.(map[string]any)
				if !ok {
					continue
				}
				name := stringField(vm, "ValueName")
				if name == "" {
					continue
				}
				label := stringField(vm, "ValueDisplayName")
				if label == "" {
					label = name
				}
				e.Options = append(e.Options, BiosOption{Value: name, Label: label})
			}
		}
		entries = append(entries, e)
	}

	rawMenus, _ := root["Menus"].([]any)
	menus := make([]biosRegMenu, 0, len(rawMenus))
	for _, raw := range rawMenus {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := normalizeMenuPath(stringField(m, "MenuPath"))
		if path == "" {
			// Some registries name the menu but not its path.
			if n := stringField(m, "MenuName"); n != "" {
				path = normalizeMenuPath("./" + n)
			}
		}
		if path == "" {
			continue
		}
		display := stringField(m, "DisplayName")
		if display == "" {
			display = stringField(m, "MenuName")
		}
		menus = append(menus, biosRegMenu{
			Path:        path,
			DisplayName: display,
			Order:       int(intField(m, "DisplayOrder")),
			Hidden:      boolField(m, "Hidden"),
		})
	}
	return entries, menus
}

// normalizeMenuPath canonicalises the "./Advanced/CPU" form so paths from
// Menus[] and from an attribute's MenuPath compare equal. An empty path
// becomes "." — the root menu — rather than being dropped, or the attributes
// carrying it would be unreachable.
func normalizeMenuPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.TrimSuffix(p, "/")
	switch {
	case p == "." || p == "./":
		return "."
	case strings.HasPrefix(p, "./"):
		return p
	case strings.HasPrefix(p, "/"):
		return "." + p
	default:
		return "./" + p
	}
}

// buildBiosMenus produces the rail: every menu the registry declared, plus
// every menu an attribute actually references (a registry may name a path in
// MenuPath without listing it under Menus), plus each one's ancestors so the
// tree has no gaps.
func buildBiosMenus(declared []biosRegMenu, attrs []BiosAttribute) []BiosMenu {
	byPath := map[string]*BiosMenu{}
	hidden := map[string]bool{}

	ensure := func(path string) *BiosMenu {
		if m, ok := byPath[path]; ok {
			return m
		}
		m := &BiosMenu{
			Path:        path,
			DisplayName: menuLeafName(path),
			Depth:       menuDepth(path),
		}
		byPath[path] = m
		return m
	}

	for _, d := range declared {
		if d.Hidden {
			hidden[d.Path] = true
			continue
		}
		m := ensure(d.Path)
		if d.DisplayName != "" {
			m.DisplayName = d.DisplayName
		}
		m.Order = d.Order
	}

	for _, a := range attrs {
		path := a.MenuPath
		if path == "" {
			path = "."
		}
		if hidden[path] {
			continue
		}
		m := ensure(path)
		m.Count++
		if a.HasPending {
			m.PendingCount++
		}
		// Materialise ancestors so "./Advanced" exists even when only
		// "./Advanced/CPU" was ever referenced.
		for _, anc := range menuAncestors(path) {
			if !hidden[anc] {
				ensure(anc)
			}
		}
	}

	out := make([]BiosMenu, 0, len(byPath))
	for _, m := range byPath {
		out = append(out, *m)
	}
	// Depth-first by path so children follow their parent in the rail, with
	// DisplayOrder breaking ties between siblings.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// The synthetic Unregistered menu always sorts last: it is a
		// leftovers bucket, not part of the firmware's own menu structure.
		if ua, ub := a.Path == unregisteredMenuPath, b.Path == unregisteredMenuPath; ua != ub {
			return ub
		}
		ai, bi := menuSortKey(a, byPath), menuSortKey(b, byPath)
		return ai < bi
	})
	return out
}

// menuSortKey builds a path-ordered key: each segment contributes its
// DisplayOrder (when the registry gave one) followed by its name, so siblings
// order by the firmware's intent and children always trail their parent.
func menuSortKey(m BiosMenu, byPath map[string]*BiosMenu) string {
	segs := menuSegments(m.Path)
	var b strings.Builder
	path := "."
	for _, s := range segs {
		path += "/" + s
		order := 0
		name := s
		if p, ok := byPath[path]; ok {
			order, name = p.Order, p.DisplayName
		}
		fmt.Fprintf(&b, "%08d\x00%s\x00", order, strings.ToLower(name))
	}
	return b.String()
}

func menuSegments(path string) []string {
	p := strings.TrimPrefix(normalizeMenuPath(path), "./")
	if p == "" || p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

func menuDepth(path string) int { return len(menuSegments(path)) }

func menuLeafName(path string) string {
	segs := menuSegments(path)
	if len(segs) == 0 {
		return "Main"
	}
	return segs[len(segs)-1]
}

// menuAncestors lists the parent paths of path, nearest first.
func menuAncestors(path string) []string {
	segs := menuSegments(path)
	out := make([]string, 0, len(segs))
	for i := len(segs) - 1; i > 0; i-- {
		out = append(out, "./"+strings.Join(segs[:i], "/"))
	}
	return out
}

// sortBiosAttributes orders within a menu by the registry's DisplayOrder, then
// by label, so a registry that sets no order still renders deterministically
// rather than in Go map order.
func sortBiosAttributes(attrs []BiosAttribute) {
	sort.SliceStable(attrs, func(i, j int) bool {
		a, b := attrs[i], attrs[j]
		if a.MenuPath != b.MenuPath {
			return a.MenuPath < b.MenuPath
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return strings.ToLower(a.Label()) < strings.ToLower(b.Label())
	})
}

// ── staging ─────────────────────────────────────────────────────────────

// BiosStageResult reports what a stage submission did, so the UI can say it.
type BiosStageResult struct {
	// Staged is the attributes whose value now differs from the live one.
	Staged []string
	// Reverted is the attributes whose submitted value matched the live one
	// and were therefore dropped from the pending set.
	Reverted []string
	// Errors maps an attribute name to why its submitted value was rejected.
	Errors map[string]string
}

// Changed reports whether anything about the pending set actually moved.
func (r BiosStageResult) Changed() bool { return len(r.Staged) > 0 || len(r.Reverted) > 0 }

// StageBiosAttributes merges submitted values into the pending set.
//
// vals maps attribute name to its raw submitted string; names lists the
// attributes the submitting form owned. The two are separate because an
// unchecked switch submits nothing at all: without the roster, "boolean turned
// off" and "attribute not on this form" are indistinguishable, and the off
// switch would silently never stage.
//
// Values equal to the live one are removed from the stage rather than staged,
// so the pending set stays a description of what will change. Attributes not
// named in names are left alone — that is what lets a UI submit one menu at a
// time without discarding another menu's staged changes.
func StageBiosAttributes(names []string, vals map[string]string) BiosStageResult {
	view := BiosSnapshot()
	byName := make(map[string]BiosAttribute, len(view.Attributes))
	for _, a := range view.Attributes {
		byName[a.Name] = a
	}

	pending := hostBiosPending()
	if pending == nil {
		pending = map[string]any{}
	}
	res := BiosStageResult{Errors: map[string]string{}}

	for _, name := range names {
		attr, known := byName[name]
		if !known {
			res.Errors[name] = "no such attribute"
			continue
		}
		if attr.ReadOnly {
			// Not an error worth surfacing: a read-only row renders no
			// editor, so this only happens on a hand-crafted POST.
			continue
		}
		// An empty password field means "leave it alone", not "set it to the
		// empty string". A password control is never seeded with the current
		// value (there is nothing safe to seed it with), so every submit of a
		// form containing one carries it back empty — without this, merely
		// saving an unrelated attribute on the same menu would stage a blank
		// administrator password for the host to apply at its next boot.
		// Clearing a password deliberately is not offered here; the asymmetry
		// between "cannot clear from this screen" and "silently locked the
		// operator out of firmware setup" decides it.
		if attr.Type == BiosTypePassword && strings.TrimSpace(vals[name]) == "" {
			continue
		}
		typed, err := biosCoerce(attr, vals[name])
		if err != nil {
			res.Errors[name] = err.Error()
			continue
		}
		if biosValuesEqual(typed, attr.Current) {
			if _, staged := pending[name]; staged {
				delete(pending, name)
				res.Reverted = append(res.Reverted, name)
			}
			continue
		}
		if prev, staged := pending[name]; staged && biosValuesEqual(prev, typed) {
			continue // already staged at this value
		}
		pending[name] = typed
		res.Staged = append(res.Staged, name)
	}

	if res.Changed() {
		setHostBiosPending(pending)
	}
	return res
}

// DiscardBiosPending clears the staged set. Distinct from staging an empty
// map only in intent, but the intent is worth a named call: this is the
// operator abandoning changes, not the host reporting it applied them.
func DiscardBiosPending() { setHostBiosPending(map[string]any{}) }

// DiscardBiosAttribute drops one attribute from the staged set.
func DiscardBiosAttribute(name string) bool {
	pending := hostBiosPending()
	if _, ok := pending[name]; !ok {
		return false
	}
	delete(pending, name)
	setHostBiosPending(pending)
	return true
}

// ── value handling ──────────────────────────────────────────────────────

// biosCoerce converts a submitted form string to the JSON type the attribute
// declares. This is the whole reason the registry is consulted on write: the
// host's firmware reads the staged object and expects `true`, not `"true"`,
// and `4096`, not `"4096"`. An attribute with no registry entry is coerced to
// the type its current value already has, which keeps a re-submitted value the
// same shape the host reported.
func biosCoerce(a BiosAttribute, raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	switch a.Type {
	case BiosTypeBoolean:
		// An unchecked switch submits nothing; absence is false.
		return raw != "" && raw != "false" && raw != "0", nil

	case BiosTypeInteger:
		if raw == "" {
			return nil, fmt.Errorf("a value is required")
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("must be a whole number")
		}
		if a.LowerBound != nil && n < *a.LowerBound {
			return nil, fmt.Errorf("must be at least %d", *a.LowerBound)
		}
		if a.UpperBound != nil && n > *a.UpperBound {
			return nil, fmt.Errorf("must be at most %d", *a.UpperBound)
		}
		return n, nil

	case BiosTypeEnumeration:
		if len(a.Options) == 0 {
			return raw, nil
		}
		for _, o := range a.Options {
			if o.Value == raw {
				return raw, nil
			}
		}
		return nil, fmt.Errorf("%q is not an allowed value", raw)

	case BiosTypeString, BiosTypePassword:
		if a.MinLength != nil && int64(len(raw)) < *a.MinLength {
			return nil, fmt.Errorf("must be at least %d characters", *a.MinLength)
		}
		if a.MaxLength != nil && int64(len(raw)) > *a.MaxLength {
			return nil, fmt.Errorf("must be at most %d characters", *a.MaxLength)
		}
		return raw, nil
	}
	return raw, nil
}

// biosInferType picks a control for an attribute the registry never described,
// from the JSON type of the value the host reported for it.
func biosInferType(v any) string {
	switch n := v.(type) {
	case bool:
		return BiosTypeBoolean
	case float64, int, int64:
		return BiosTypeInteger
	case json.Number:
		if _, err := n.Int64(); err == nil {
			return BiosTypeInteger
		}
		return BiosTypeString
	default:
		return BiosTypeString
	}
}

// biosValuesEqual compares two attribute values across the type differences
// that survive a JSON round-trip — a staged int64 against a reported float64,
// a json.Number against either. Without this every staged integer would look
// different from its own live value on the next render and never clear.
func biosValuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if ab, ok := biosToBool(a); ok {
		if bb, ok2 := biosToBool(b); ok2 {
			return ab == bb
		}
		return false
	}
	if af, ok := biosToFloat(a); ok {
		if bf, ok2 := biosToFloat(b); ok2 {
			return af == bf
		}
		return false
	}
	return biosValueString(a) == biosValueString(b)
}

func biosToBool(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		// A host that reports "Enabled"/"Disabled" for a Boolean-typed
		// attribute is not hypothetical; treat those as the booleans they are
		// so a switch does not read every such value as true.
		switch strings.ToLower(b) {
		case "true", "enabled", "yes", "on":
			return true, true
		case "false", "disabled", "no", "off":
			return false, true
		}
	}
	return false, false
}

func biosToFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// biosValueString renders a value for display or for seeding a control.
// Integers render without a decimal point: a float64 4096 out of JSON must
// show as "4096", not "4096.000000".
func biosValueString(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case bool:
		return strconv.FormatBool(n)
	case json.Number:
		return n.String()
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case float32:
		return biosValueString(float64(n))
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// ── small helpers ───────────────────────────────────────────────────────

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func boolField(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

func intField(m map[string]any, key string) int64 {
	if p := optIntField(m, key); p != nil {
		return *p
	}
	return 0
}

// optIntField distinguishes "the registry declared 0" from "the registry
// declared nothing", which matters for bounds: a LowerBound of 0 is a real
// constraint, and treating it as absent would let negatives through.
func optIntField(m map[string]any, key string) *int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	if f, ok := biosToFloat(v); ok {
		n := int64(f)
		return &n
	}
	if s, ok := v.(string); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return &n
		}
	}
	return nil
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
