package redfish

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEmbeddedOpenAPI_YAMLParses confirms the YAML compiled into the
// binary is valid (catches typos / indentation regressions at test time
// instead of at first request).
func TestEmbeddedOpenAPI_YAMLParses(t *testing.T) {
	spec := OpenAPIYAML()
	if len(spec) == 0 {
		t.Fatal("OpenAPIYAML() returned an empty spec")
	}
	var doc map[string]any
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if doc["openapi"] == nil {
		t.Error("openapi.yaml missing 'openapi' top-level key")
	}
	if doc["paths"] == nil {
		t.Error("openapi.yaml missing 'paths'")
	}
}
