package redfish

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
)

// Every response advertises @odata.context pointing into $metadata, so the
// document has to exist.
func TestMetadataDocumentResolves(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := NewService(&deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}, slog.New(slog.DiscardHandler)),
		Firmware: firmware.NewController(&config.Config{}, slog.New(slog.DiscardHandler)),
	})
	r := gin.New()
	r.GET(metadataPath, svc.GetMetadata)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, metadataPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", metadataPath, w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"edmx:Edmx", "EntityContainer", "ServiceRoot", "EthernetInterface"} {
		if !strings.Contains(body, want) {
			t.Errorf("$metadata is missing %q", want)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("Content-Type = %q, want XML", ct)
	}
}
