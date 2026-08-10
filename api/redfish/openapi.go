package redfish

// openapi.go serves the OpenAPI 3.1 specification for the Redfish surface
// this BMC implements. The spec is authored in YAML (embedded in
// pkg/redfish) — that's the canonical form a human edits. JSON is produced
// on demand from the same source so clients that prefer JSON aren't left
// out.
//
// Endpoints (wired in Register):
//   GET /redfish/v1/openapi.yaml — the spec, served as application/yaml
//   GET /redfish/v1/openapi.json — same spec, converted to JSON
//
// A custom templ-rendered docs page is served at /docs (see
// ui/pages/api_docs.templ); SwaggerUI is no longer used.
//
// Both endpoints are public (no auth) so a tool can discover the
// surface before authenticating.

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/pi-bmc/nanokvm-app/pkg/redfish"
)

// jsonOnce / cachedJSON memoise the YAML→JSON conversion so we only do
// it once per process. The spec is static for the lifetime of a binary.
var (
	jsonOnce   sync.Once
	cachedJSON []byte
	errCached  error
)

// GetOpenAPIYAML serves the OpenAPI spec verbatim. The spec is an
// immutable string, so this writes it without copying.
func (s *Service) GetOpenAPIYAML(c *gin.Context) {
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", redfish.OpenAPIYAML())
}

// GetOpenAPIJSON serves the OpenAPI spec as JSON. Parses YAML once
// (sync.Once) and serves the cached bytes on every subsequent call.
func (s *Service) GetOpenAPIJSON(c *gin.Context) {
	jsonOnce.Do(func() {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(redfish.OpenAPIYAML()), &doc); err != nil {
			errCached = err
			return
		}
		// json.Marshal can't represent map[any]any (which gopkg.in/yaml.v3
		// produces for nested maps). We unmarshalled into map[string]any
		// at the top level, but nested maps may still be map[any]any
		// depending on the YAML doc — normalise before marshalling.
		normalised := redfish.NormaliseYAMLMaps(doc)
		out, err := json.Marshal(normalised)
		if err != nil {
			errCached = err
			return
		}
		cachedJSON = out
	})
	if errCached != nil {
		redfishErrorResponse(c, http.StatusInternalServerError, errCached.Error())
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", cachedJSON)
}
