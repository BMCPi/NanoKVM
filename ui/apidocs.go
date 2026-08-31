package ui

// apidocs.go bridges the OpenAPI spec served by the redfish package and
// the pages.APIDocsPage renderer. The parsed model is cached for
// the process lifetime — the spec is embedded so it's static for the
// lifetime of the binary.

import (
	"sync"

	"github.com/pi-bmc/nanokvm-app/pkg/protocol/redfish"
	"github.com/pi-bmc/nanokvm-app/ui/pages"
)

var (
	apiDocsOnce  sync.Once
	apiDocsModel pages.APIDocsModel
	errAPIDocs   error
)

// loadAPIDocsModel parses redfish.OpenAPIYAML() into a renderable model.
// Parses lazily on first call, then caches the result. Safe for
// concurrent use.
func loadAPIDocsModel() (pages.APIDocsModel, error) {
	apiDocsOnce.Do(func() {
		apiDocsModel, errAPIDocs = pages.LoadAPIDocs(redfish.OpenAPIYAML())
	})
	return apiDocsModel, errAPIDocs
}
