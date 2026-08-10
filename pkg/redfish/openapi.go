// Package redfish holds the shared Redfish artifacts other packages
// consume: the embedded OpenAPI 3.1 specification (the canonical,
// human-edited YAML) and the YAML normalisation helper. The HTTP service
// that implements the Redfish surface lives in api/redfish; the docs page
// renderer (ui/pages) parses the same embedded spec.
package redfish

import (
	_ "embed"
)

// The spec is embedded as a string so it is immutable by construction —
// callers can share it without defensive copies.
//
//go:embed openapi.yaml
var openAPIYAML string

// OpenAPIYAML returns the embedded spec. Exported so other packages
// (the api/redfish handlers and the ui docs-page renderer) can serve and
// parse the same source.
func OpenAPIYAML() []byte {
	return []byte(openAPIYAML)
}

// NormaliseYAMLMaps walks a decoded YAML value and converts every
// map[any]any to map[string]any so encoding/json can handle it.
func NormaliseYAMLMaps(in any) any {
	switch v := in.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			out[k] = NormaliseYAMLMaps(vv)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			if ks, ok := k.(string); ok {
				out[ks] = NormaliseYAMLMaps(vv)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, vv := range v {
			out[i] = NormaliseYAMLMaps(vv)
		}
		return out
	default:
		return v
	}
}
