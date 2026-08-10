package redfish

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// TestEmbeddedOpenAPI_YAMLParses confirms the YAML compiled into the
// binary is valid (catches typos / indentation regressions at test time
// instead of at first request).
func TestEmbeddedOpenAPI_YAMLParses(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(openAPIYAML, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if doc["openapi"] == nil {
		t.Error("openapi.yaml missing 'openapi' top-level key")
	}
	if doc["paths"] == nil {
		t.Error("openapi.yaml missing 'paths'")
	}
}

// TestGetOpenAPIYAML_ServesEmbeddedBytes verifies the YAML handler
// returns the spec verbatim with the right content type.
func TestGetOpenAPIYAML_ServesEmbeddedBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService()
	r.GET("/openapi.yaml", svc.GetOpenAPIYAML)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/yaml") {
		t.Errorf("Content-Type = %q; want application/yaml*", w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), "openapi:") {
		t.Errorf("response body missing 'openapi:' line")
	}
}

// TestGetOpenAPIJSON_RoundTrip verifies the YAML→JSON conversion
// produces parseable JSON whose structure matches the source.
func TestGetOpenAPIJSON_RoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService()
	r.GET("/openapi.json", svc.GetOpenAPIJSON)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q; want application/json*", w.Header().Get("Content-Type"))
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got["openapi"] == nil {
		t.Error("JSON output missing 'openapi' top-level key")
	}
	if paths, ok := got["paths"].(map[string]any); !ok || paths["/redfish/v1/Systems/1/Bios"] == nil {
		t.Error("JSON output missing Bios path entry")
	}
}

// The spec is served to clients, so it must document the paths we actually
// route. Before the migration it already specified the trailing-slash root and
// string-typed BitRate while the code emitted neither — this pins them
// together so they cannot drift apart again silently.
func TestOpenAPI_DocumentsCanonicalServiceRoot(t *testing.T) {
	var doc struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openAPIYAML, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}

	if _, ok := doc.Paths[ServiceRootPath]; !ok {
		t.Errorf("openapi.yaml does not document the canonical root %q", ServiceRootPath)
	}
	// The compatibility alias must stay documented too.
	if _, ok := doc.Paths["/redfish/v1"]; !ok {
		t.Error("openapi.yaml dropped the bare /redfish/v1 alias")
	}

	// Every documented Redfish path must sit under the service root, aside
	// from the version pointer above it and the bare-root alias.
	for p := range doc.Paths {
		if p == "/redfish" || p == strings.TrimSuffix(ServiceRootPath, "/") {
			continue
		}
		if !strings.HasPrefix(p, ServiceRootPath) {
			t.Errorf("documented path %q is not under %q", p, ServiceRootPath)
		}
	}
}

func TestOpenAPIYAML_Exported(t *testing.T) {
	// The exported byte slice is what the templates layer parses to
	// render the custom docs page; make sure it's non-empty and a
	// defensive copy (mutating the returned slice mustn't poke at the
	// embedded source).
	a := OpenAPIYAML()
	b := OpenAPIYAML()
	if len(a) == 0 {
		t.Fatal("OpenAPIYAML() returned empty bytes")
	}
	if len(a) > 0 {
		a[0] ^= 0xff
		if b[0] == a[0] {
			t.Error("OpenAPIYAML() returned an aliased slice; want a defensive copy")
		}
	}
}
