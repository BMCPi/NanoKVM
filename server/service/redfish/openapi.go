package redfish

// openapi.go serves the OpenAPI 3.1 specification for the Redfish surface
// this BMC implements. The spec is authored in YAML (openapi.yaml,
// embedded below) — that's the canonical form a human edits. JSON is
// produced on demand from the same source so clients that prefer JSON
// aren't left out.
//
// Endpoints (wired in server/router/redfish.go):
//   GET /redfish/v1/openapi.yaml — the spec, served as application/yaml
//   GET /redfish/v1/openapi.json — same spec, converted to JSON
//
// A custom templui-based docs page is served at /docs (see
// server/pages/api_docs.templ); SwaggerUI is no longer used.
//
// Both endpoints are public (no auth) so a tool can discover the
// surface before authenticating.

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var openAPIYAML []byte

// OpenAPIYAML returns the embedded spec bytes. Exported so other
// packages (e.g. the templates layer that renders the docs page) can
// parse the same source the HTTP handlers serve.
func OpenAPIYAML() []byte {
	out := make([]byte, len(openAPIYAML))
	copy(out, openAPIYAML)
	return out
}

// jsonOnce / cachedJSON memoise the YAML→JSON conversion so we only do
// it once per process. The spec is static for the lifetime of a binary.
var (
	jsonOnce   sync.Once
	cachedJSON []byte
	errCached  error
)

// GetOpenAPIYAML serves the OpenAPI spec verbatim.
func (s *Service) GetOpenAPIYAML(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", openAPIYAML)
}

// GetOpenAPIJSON serves the OpenAPI spec as JSON. Parses YAML once
// (sync.Once) and serves the cached bytes on every subsequent call.
func (s *Service) GetOpenAPIJSON(c *gin.Context) {
	jsonOnce.Do(func() {
		var doc map[string]any
		if err := yaml.Unmarshal(openAPIYAML, &doc); err != nil {
			errCached = err
			return
		}
		// json.Marshal can't represent map[any]any (which gopkg.in/yaml.v3
		// produces for nested maps). We unmarshalled into map[string]any
		// at the top level, but nested maps may still be map[any]any
		// depending on the YAML doc — normalise before marshalling.
		normalised := normaliseYAMLMaps(doc)
		out, err := json.Marshal(normalised)
		if err != nil {
			errCached = err
			return
		}
		cachedJSON = out
	})
	if errCached != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errCached.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", cachedJSON)
}

// normaliseYAMLMaps walks a decoded YAML value and converts every
// map[any]any to map[string]any so encoding/json can handle it.
func normaliseYAMLMaps(in any) any {
	switch v := in.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			out[k] = normaliseYAMLMaps(vv)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			if ks, ok := k.(string); ok {
				out[ks] = normaliseYAMLMaps(vv)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, vv := range v {
			out[i] = normaliseYAMLMaps(vv)
		}
		return out
	default:
		return v
	}
}
