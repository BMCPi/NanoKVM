package pages

// api_docs_loader.go parses the embedded OpenAPI 3.x spec
// (pkg/redfish/openapi.yaml) into a flat data model that the
// APIDocsPage template can render. We don't use a full OpenAPI library
// here — the spec is finite and project-owned, so manual extraction
// keeps the dep surface minimal.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/protocol/redfish"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"gopkg.in/yaml.v3"
)

// markdownRenderer is the shared goldmark instance used to convert
// description strings (from the OpenAPI spec) into HTML. GFM is
// enabled so the spec can use tables and auto-linked URLs the same as
// it does in GitHub-rendered READMEs.
var markdownRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
)

// RenderMarkdown converts s to HTML using the shared goldmark renderer.
// Returns the empty string when s is empty. Errors fall back to the
// HTML-escaped original so a malformed description never crashes the
// page.
func RenderMarkdown(s string) string {
	if s == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := markdownRenderer.Convert([]byte(s), &buf); err != nil {
		return strings.ReplaceAll(strings.ReplaceAll(s, "<", "&lt;"), ">", "&gt;")
	}
	return buf.String()
}

// APIDocsModel is the page-ready view of the OpenAPI spec.
type APIDocsModel struct {
	Title       string
	Version     string
	Summary     string
	Description string

	// Tags are the navigation groups (sidebar entries), in the order
	// declared in the spec's top-level `tags` array.
	Tags []APIDocsTag

	// ByTag maps a tag name → its operations (in source order). Tags
	// with no operations are still listed in Tags but absent from ByTag.
	ByTag map[string][]APIDocsOperation
}

// APIDocsTag is one navigation group.
type APIDocsTag struct {
	Name        string
	Description string
	Anchor      string // HTML id used by the in-page nav link
	Count       int    // number of operations under this tag
}

// APIDocsOperation is a single Method+Path entry with everything the
// template needs to render its card.
type APIDocsOperation struct {
	Method      string // GET / POST / PATCH / etc. — upper-case
	Path        string
	Summary     string
	Description string
	Tag         string
	Anchor      string // HTML id for direct deep-linking
	Parameters  []APIDocsParam
	RequestBody *APIDocsBody // nil when the operation has no body
	Responses   []APIDocsResponse
	PublicAuth  bool // true when the operation declares `security: []`
}

// APIDocsParam is one path/query/header parameter.
type APIDocsParam struct {
	Name        string
	In          string // "path" | "query" | "header" | "cookie"
	Required    bool
	Description string
	Type        string // primitive type from the schema, when present
}

// APIDocsBody describes a request body (always one content type per
// operation — we use the first if multiple are declared).
type APIDocsBody struct {
	ContentType string
	Required    bool
	Example     string         // pretty-printed JSON when an `example` was set
	Schema      *APIDocsSchema // resolved schema, nil when absent
}

// APIDocsResponse is one (status → description) pair, optionally with a
// pretty-printed JSON example for the right-column "Response samples" panel.
type APIDocsResponse struct {
	Status      string // "200", "default", etc.
	Description string
	ContentType string         // first content type with an example, when present
	Example     string         // pretty-printed JSON example, empty when absent
	Schema      *APIDocsSchema // resolved schema, nil when absent
}

// APIDocsSchema is a Redoc-style schema tree. Each node carries its
// JSON-schema metadata plus child nodes for object properties / array
// items / oneOf variants. Cycles in the OpenAPI doc are broken by
// returning a leaf with RefName set and Properties left nil.
type APIDocsSchema struct {
	Type        string // "object" | "array" | "string" | "integer" | "number" | "boolean" | "" for refs/composition
	Format      string // e.g. "int32", "date-time"
	Description string
	Example     string // raw value rendered as text
	Default     string
	Enum        []string
	// RefName is the component-schemas key when this node originated as
	// a $ref. Used for headers / cycle-break leaves.
	RefName string
	// Nullable is set when the schema's type was declared as
	// [Foo, "null"] (OpenAPI 3.1 / JSON Schema style).
	Nullable bool

	// Object-shape fields.
	Properties []APIDocsSchemaProp
	Required   map[string]bool
	// AdditionalProperties carries the value schema when the object
	// allows extra keys, e.g. `additionalProperties: { type: string }`.
	AdditionalProperties *APIDocsSchema

	// Array-shape fields.
	Items *APIDocsSchema

	// Composition. Rendered as a "one of" pill list with each variant
	// expandable. allOf is pre-merged into Properties during build.
	OneOf []*APIDocsSchema
}

// APIDocsSchemaProp is one named property on an object schema.
type APIDocsSchemaProp struct {
	Name     string
	Required bool
	Schema   *APIDocsSchema
}

// resolver carries the components maps so $ref strings can be resolved
// against them while building schemas and responses.
type resolver struct {
	schemas   map[string]any // components.schemas
	responses map[string]any // components.responses
}

func newResolver(doc map[string]any) *resolver {
	r := &resolver{}
	if comp, ok := doc["components"].(map[string]any); ok {
		r.schemas, _ = comp["schemas"].(map[string]any)
		r.responses, _ = comp["responses"].(map[string]any)
	}
	return r
}

// LoadAPIDocs parses an OpenAPI YAML document into APIDocsModel.
func LoadAPIDocs(yamlBytes []byte) (APIDocsModel, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		return APIDocsModel{}, fmt.Errorf("parse openapi: %w", err)
	}
	doc, _ := redfish.NormaliseYAMLMaps(raw).(map[string]any)

	res := newResolver(doc)
	m := APIDocsModel{ByTag: map[string][]APIDocsOperation{}}

	if info, ok := doc["info"].(map[string]any); ok {
		m.Title, _ = info["title"].(string)
		m.Version, _ = info["version"].(string)
		m.Summary, _ = info["summary"].(string)
		m.Description, _ = info["description"].(string)
	}

	if tags, ok := doc["tags"].([]any); ok {
		for _, t := range tags {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tm["name"].(string)
			desc, _ := tm["description"].(string)
			if name == "" {
				continue
			}
			m.Tags = append(m.Tags, APIDocsTag{
				Name:        name,
				Description: desc,
				Anchor:      "tag-" + slugify(name),
			})
		}
	}

	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		return m, nil
	}

	// Iterate paths in alphabetical order for deterministic output.
	pathKeys := make([]string, 0, len(paths))
	for k := range paths {
		pathKeys = append(pathKeys, k)
	}
	sort.Strings(pathKeys)

	// Method order matches Swagger UI conventions (read-then-write).
	methodOrder := []string{"get", "post", "patch", "put", "delete", "head", "options"}

	for _, path := range pathKeys {
		pi, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		for _, method := range methodOrder {
			opMap, ok := pi[method].(map[string]any)
			if !ok {
				continue
			}
			op := newAPIOp(res, method, path, pi, opMap)
			m.ByTag[op.Tag] = append(m.ByTag[op.Tag], op)
		}
	}

	// Ensure every tag with operations is represented in Tags, and that
	// each Tag.Count reflects the actual operation count. If a tag was
	// referenced by an operation but not declared in the spec's tags
	// array, append it at the end so it still gets a nav entry.
	declared := map[string]int{}
	for i, t := range m.Tags {
		declared[t.Name] = i
	}
	for tagName := range m.ByTag {
		if _, ok := declared[tagName]; !ok && tagName != "" {
			m.Tags = append(m.Tags, APIDocsTag{
				Name:   tagName,
				Anchor: "tag-" + slugify(tagName),
			})
		}
	}
	for i := range m.Tags {
		m.Tags[i].Count = len(m.ByTag[m.Tags[i].Name])
	}

	return m, nil
}

func newAPIOp(res *resolver, method, path string, pi, opMap map[string]any) APIDocsOperation {
	op := APIDocsOperation{
		Method: strings.ToUpper(method),
		Path:   path,
		Anchor: "op-" + slugify(method+"-"+path),
	}
	op.Summary, _ = opMap["summary"].(string)
	op.Description, _ = opMap["description"].(string)

	if tagList, ok := opMap["tags"].([]any); ok && len(tagList) > 0 {
		if t, ok := tagList[0].(string); ok {
			op.Tag = t
		}
	}
	if op.Tag == "" {
		op.Tag = "Other"
	}

	// `security: []` (empty array, not absent) is the OpenAPI convention
	// for "explicitly public — overrides the doc's top-level security".
	if sec, ok := opMap["security"].([]any); ok && len(sec) == 0 {
		op.PublicAuth = true
	}

	op.Parameters = extractAPIParams(pi, opMap)
	op.RequestBody = extractAPIBody(res, opMap)
	op.Responses = extractAPIResponses(res, opMap)
	return op
}

// extractAPIParams pulls parameters from both the path-level
// `parameters` (applies to every method on the path) and the per-op
// `parameters`. $ref entries are skipped (we don't have a $ref resolver
// here and our spec uses very few of them at the param level).
func extractAPIParams(pi, op map[string]any) []APIDocsParam {
	var out []APIDocsParam
	for _, src := range []any{pi["parameters"], op["parameters"]} {
		list, ok := src.([]any)
		if !ok {
			continue
		}
		for _, p := range list {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if _, hasRef := pm["$ref"]; hasRef {
				continue
			}
			ap := APIDocsParam{}
			ap.Name, _ = pm["name"].(string)
			ap.In, _ = pm["in"].(string)
			ap.Required, _ = pm["required"].(bool)
			ap.Description, _ = pm["description"].(string)
			if sch, ok := pm["schema"].(map[string]any); ok {
				if t, ok := sch["type"].(string); ok {
					ap.Type = t
				}
			}
			if ap.Name != "" {
				out = append(out, ap)
			}
		}
	}
	return out
}

func extractAPIBody(res *resolver, op map[string]any) *APIDocsBody {
	body, ok := op["requestBody"].(map[string]any)
	if !ok {
		return nil
	}
	out := &APIDocsBody{}
	out.Required, _ = body["required"].(bool)
	if content, ok := body["content"].(map[string]any); ok {
		ct, vm := firstContent(content)
		out.ContentType = ct
		out.Example, out.Schema = exampleAndSchema(res, vm)
	}
	return out
}

// exampleAndSchema pulls the "example" and "schema" members out of an
// OpenAPI media-type object (vm, the value firstContent returns alongside
// its content type). vm is nil when the content map had no entries, in
// which case both results are zero, matching the nested checks this
// factors out of extractAPIBody and applyResponseContent.
func exampleAndSchema(res *resolver, vm map[string]any) (example string, schema *APIDocsSchema) {
	if vm == nil {
		return "", nil
	}
	if ex, ok := vm["example"]; ok {
		if pretty, err := json.MarshalIndent(ex, "", "  "); err == nil {
			example = string(pretty)
		}
	}
	if sm, ok := vm["schema"].(map[string]any); ok {
		schema = res.buildSchema(sm, nil)
	}
	return example, schema
}

func extractAPIResponses(res *resolver, op map[string]any) []APIDocsResponse {
	responses, ok := op["responses"].(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(responses))
	for k := range responses {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]APIDocsResponse, 0, len(keys))
	for _, k := range keys {
		r, ok := responses[k].(map[string]any)
		if !ok {
			continue
		}
		// Resolve a $ref to components.responses — Redoc behaviour:
		// surface the target's description + content the same as an
		// inline response. Falls back to "(RefName)" when the target
		// can't be resolved.
		var refName string
		if ref, ok := r["$ref"].(string); ok {
			refName = lastPathSegment(ref)
			if res.responses != nil {
				if resolved, ok := res.responses[refName].(map[string]any); ok {
					r = resolved
				}
			}
		}
		ar := APIDocsResponse{Status: k}
		ar.Description, _ = r["description"].(string)
		if ar.Description == "" && refName != "" {
			ar.Description = "(" + refName + ")"
		}
		if content, ok := r["content"].(map[string]any); ok {
			ct, vm := firstContent(content)
			applyResponseContent(res, &ar, ct, vm)
		}
		out = append(out, ar)
	}
	return out
}

// applyResponseContent fills ar's ContentType/Example/Schema from vm (the
// media-type object for content type ct, as returned by firstContent).
// Mirrors the nested logic this factors out of extractAPIResponses exactly:
// ContentType is set only once, lazily, when an example or schema is
// actually found — never unconditionally the way extractAPIBody's does,
// since a response with no content must leave APIDocsResponse.ContentType
// empty rather than reporting a content type that had nothing in it.
func applyResponseContent(res *resolver, ar *APIDocsResponse, ct string, vm map[string]any) {
	if vm == nil {
		return
	}
	if ex, ok := vm["example"]; ok {
		if pretty, err := json.MarshalIndent(ex, "", "  "); err == nil {
			ar.ContentType = ct
			ar.Example = string(pretty)
		}
	}
	if sm, ok := vm["schema"].(map[string]any); ok {
		if ar.ContentType == "" {
			ar.ContentType = ct
		}
		ar.Schema = res.buildSchema(sm, nil)
	}
}

// firstContent returns the alphabetically-first content-type entry from
// an OpenAPI content map together with its descriptor. Our spec only
// uses one content type per body but the rule keeps the choice
// deterministic when more are added.
func firstContent(content map[string]any) (string, map[string]any) {
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "", nil
	}
	vm, _ := content[keys[0]].(map[string]any)
	return keys[0], vm
}

// maxSchemaDepth caps recursion when building schema trees. Our spec is
// shallow (<=4 levels) so the limit only matters as a guard against
// pathological cycles introduced later.
const maxSchemaDepth = 8

// buildSchema converts an OpenAPI schema dict into APIDocsSchema. The
// `seen` parameter is a chain of ref names already being expanded above
// this call: re-encountering one breaks the cycle by returning a leaf
// node whose RefName is set so the renderer can show
// "→ SchemaName (recursive)".
func (r *resolver) buildSchema(raw map[string]any, seen []string) *APIDocsSchema {
	if raw == nil {
		return nil
	}
	if len(seen) > maxSchemaDepth {
		return &APIDocsSchema{Description: "(max depth reached)"}
	}

	// $ref: short-circuit by recursing on the referenced schema, but
	// detect cycles via `seen`.
	if ref, ok := raw["$ref"].(string); ok {
		name := lastPathSegment(ref)
		for _, s := range seen {
			if s == name {
				return &APIDocsSchema{RefName: name}
			}
		}
		target, _ := r.schemas[name].(map[string]any)
		if target == nil {
			return &APIDocsSchema{RefName: name}
		}
		built := r.buildSchema(target, append(seen, name))
		if built != nil {
			built.RefName = name
		}
		return built
	}

	s := &APIDocsSchema{}
	s.Type, s.Nullable = schemaType(raw["type"])
	s.Format, _ = raw["format"].(string)
	s.Description, _ = raw["description"].(string)
	s.Default = stringifyScalar(raw["default"])
	if ex, ok := raw["example"]; ok {
		s.Example = stringifyScalar(ex)
	}
	if enum, ok := raw["enum"].([]any); ok {
		for _, v := range enum {
			s.Enum = append(s.Enum, stringifyScalar(v))
		}
	}

	// allOf — merge member schemas into s. This handles the common
	// "extends a base + adds properties" idiom without forcing the
	// renderer to know about composition.
	if allOf, ok := raw["allOf"].([]any); ok {
		for _, m := range allOf {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			mergeSchema(s, r.buildSchema(mm, seen))
		}
	}

	// oneOf / anyOf — surfaced as variants for the renderer to expand.
	for _, key := range []string{"oneOf", "anyOf"} {
		if list, ok := raw[key].([]any); ok {
			for _, m := range list {
				mm, ok := m.(map[string]any)
				if !ok {
					continue
				}
				s.OneOf = append(s.OneOf, r.buildSchema(mm, seen))
			}
		}
	}

	// required is a list of property names. Stash as a set so the
	// property loop below can flag entries cheaply.
	if req, ok := raw["required"].([]any); ok {
		s.Required = make(map[string]bool, len(req))
		for _, v := range req {
			if name, ok := v.(string); ok {
				s.Required[name] = true
			}
		}
	}

	if props, ok := raw["properties"].(map[string]any); ok {
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			pm, ok := props[k].(map[string]any)
			if !ok {
				continue
			}
			child := r.buildSchema(pm, seen)
			s.Properties = append(s.Properties, APIDocsSchemaProp{
				Name:     k,
				Required: s.Required[k],
				Schema:   child,
			})
		}
		if s.Type == "" {
			s.Type = "object"
		}
	}

	if ap, ok := raw["additionalProperties"]; ok {
		switch v := ap.(type) {
		case map[string]any:
			s.AdditionalProperties = r.buildSchema(v, seen)
		case bool:
			if v {
				s.AdditionalProperties = &APIDocsSchema{Type: "any"}
			}
		}
		if s.Type == "" {
			s.Type = "object"
		}
	}

	if items, ok := raw["items"].(map[string]any); ok {
		s.Items = r.buildSchema(items, seen)
		if s.Type == "" {
			s.Type = "array"
		}
	}

	return s
}

// schemaType normalises OpenAPI's two type spellings: plain string
// ("object") and the 3.1 nullable array form (["string", "null"]).
func schemaType(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		return v, false
	case []any:
		var primary string
		var nullable bool
		for _, t := range v {
			if ts, ok := t.(string); ok {
				if ts == "null" {
					nullable = true
				} else if primary == "" {
					primary = ts
				}
			}
		}
		return primary, nullable
	default:
		return "", false
	}
}

// mergeSchema folds src's relevant fields into dst — used to flatten
// `allOf` members into the parent schema.
func mergeSchema(dst, src *APIDocsSchema) {
	if src == nil {
		return
	}
	if dst.Type == "" {
		dst.Type = src.Type
	}
	if dst.Description == "" {
		dst.Description = src.Description
	}
	if src.Required != nil {
		if dst.Required == nil {
			dst.Required = map[string]bool{}
		}
		for k, v := range src.Required {
			dst.Required[k] = v
		}
	}
	for _, p := range src.Properties {
		if p.Required || dst.Required[p.Name] {
			p.Required = true
		}
		dst.Properties = append(dst.Properties, p)
	}
	if dst.Items == nil {
		dst.Items = src.Items
	}
	if dst.AdditionalProperties == nil {
		dst.AdditionalProperties = src.AdditionalProperties
	}
}

// stringifyScalar formats an arbitrary JSON-ish value as a short string
// for the schema renderer. Objects / arrays fall back to JSON.
func stringifyScalar(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		// YAML decodes integers like 115200 as float64; keep them
		// integer-shaped when there's no fractional part.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return ""
	}
}

// FirstResponseSample returns the first response that carries an example
// payload (preferring 2xx). Empty status means no sample is available.
func (op APIDocsOperation) FirstResponseSample() APIDocsResponse {
	for _, r := range op.Responses {
		if r.Example != "" && strings.HasPrefix(r.Status, "2") {
			return r
		}
	}
	for _, r := range op.Responses {
		if r.Example != "" {
			return r
		}
	}
	return APIDocsResponse{}
}

// slugify turns "Server Overview" / "/Systems/1/Bios/Settings" into
// HTML-id-friendly strings. Lower-case, replace punctuation with hyphens,
// collapse runs, trim edges.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(
		"/", "-",
		" ", "-",
		".", "-",
		"{", "",
		"}", "",
		":", "",
		"#", "",
	).Replace(s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// needsRecursion reports whether the schema has child structure worth
// rendering inline (object properties, array items, oneOf variants,
// additional-properties value). Primitive leaves with only enum/example
// can use the inline meta renderer.
func needsRecursion(s *APIDocsSchema) bool {
	if s == nil {
		return false
	}
	return len(s.Properties) > 0 || s.Items != nil || len(s.OneOf) > 0 || s.AdditionalProperties != nil
}

// typeLabel produces the small text label rendered in the type pill —
// e.g. "string", "array of object", "object", "string · int32",
// "string?" when nullable. Falls back to the ref name when the schema
// was a $ref cycle leaf.
func typeLabel(s *APIDocsSchema) string {
	if s == nil {
		return ""
	}
	if s.Type == "" && s.RefName != "" {
		return "→ " + s.RefName
	}
	t := s.Type
	if t == "" {
		if len(s.OneOf) > 0 {
			t = "one of"
		} else {
			t = "any"
		}
	}
	if t == "array" {
		inner := "any"
		if s.Items != nil {
			inner = typeLabel(s.Items)
		}
		t = "array of " + inner
	}
	if s.Format != "" {
		t = t + " · " + s.Format
	}
	if s.Nullable {
		t += " · nullable"
	}
	return t
}

// shortMethod returns a fixed-width abbreviation for sidebar method
// tags. Keeps long operation summaries from wrapping next to wider verbs
// like DELETE / OPTIONS.
func shortMethod(method string) string {
	switch method {
	case "DELETE":
		return "DEL"
	case "OPTIONS":
		return "OPT"
	case "PATCH":
		return "PAT"
	default:
		return method
	}
}

// hasParamsIn reports whether params contains at least one entry whose
// In field equals loc ("path", "query", "header", "cookie").
func hasParamsIn(params []APIDocsParam, loc string) bool {
	for _, p := range params {
		if p.In == loc {
			return true
		}
	}
	return false
}

// filterParams returns the subset of params whose In field equals loc.
// Preserves source order so docs match the spec's parameter ordering.
func filterParams(params []APIDocsParam, loc string) []APIDocsParam {
	out := make([]APIDocsParam, 0, len(params))
	for _, p := range params {
		if p.In == loc {
			out = append(out, p)
		}
	}
	return out
}

// MethodColorClass returns Tailwind utility classes for an HTTP method
// badge. Exposed for the template — keeping the colour map next to the
// model file rather than inside templ so adding a method is a one-liner.
func MethodColorClass(method string) string {
	switch method {
	case "GET":
		return "bg-blue-500/15 text-blue-500 border border-blue-500/30"
	case "POST":
		return "bg-green-500/15 text-green-500 border border-green-500/30"
	case "PATCH":
		return "bg-yellow-500/15 text-yellow-500 border border-yellow-500/30"
	case "PUT":
		return "bg-orange-500/15 text-orange-500 border border-orange-500/30"
	case "DELETE":
		return "bg-destructive/15 text-destructive border border-destructive/30"
	default:
		return "bg-muted text-foreground border border-border"
	}
}

// StatusColorClass returns Tailwind classes for response-status badges.
func StatusColorClass(status string) string {
	if strings.HasPrefix(status, "2") {
		return "bg-green-500/15 text-green-500"
	}
	if strings.HasPrefix(status, "3") {
		return "bg-blue-500/15 text-blue-500"
	}
	if strings.HasPrefix(status, "4") {
		return "bg-yellow-500/15 text-yellow-500"
	}
	if strings.HasPrefix(status, "5") {
		return "bg-destructive/15 text-destructive"
	}
	return "bg-muted text-foreground"
}
