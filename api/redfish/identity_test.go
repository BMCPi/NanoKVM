package redfish

// identity_test.go pins the one property that makes a discovered BMC the
// same BMC tomorrow. An inventory keyed on a UUID that changes per boot
// registers a new node every scan.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

func TestManagerUUIDIsStableAndWellFormed(t *testing.T) {
	first := managerUUID()
	if first == "" {
		t.Skip("no machine-id and no NIC: nothing stable to derive an identity from")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("managerUUID() = %q, not a UUID: %v", first, err)
	}
	if second := managerUUID(); second != first {
		t.Errorf("managerUUID() changed between calls: %q then %q", first, second)
	}
}

// The seed must come from something that outlives the process. Deriving it
// from the same inputs must reproduce the same UUID.
func TestBMCIdentitySeedIsDeterministic(t *testing.T) {
	seed := bmcIdentitySeed()
	if seed == "" {
		t.Skip("no stable identity source on this host")
	}
	if !strings.HasPrefix(seed, "machine-id:") && !strings.HasPrefix(seed, "mac:") {
		t.Errorf("seed %q is from neither machine-id nor a MAC", seed)
	}
	if again := bmcIdentitySeed(); again != seed {
		t.Errorf("seed is not deterministic: %q then %q", seed, again)
	}
}

// ServiceRoot and Manager must agree: a client that keys off either one has
// to land on the same device.
func TestServiceRootAndManagerShareUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	svc := NewService(&deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}),
		Firmware: firmware.NewController(cfg),
	})
	r := gin.New()
	r.GET(ServiceRootPath, svc.GetServiceRoot)
	r.GET(managerPath, svc.GetManager)

	root := mustGetJSON(t, r, ServiceRootPath)
	mgr := mustGetJSON(t, r, managerPath)

	if managerUUID() == "" {
		t.Skip("no stable identity source on this host")
	}
	if root["UUID"] != mgr["UUID"] {
		t.Errorf("ServiceRoot UUID %v != Manager UUID %v", root["UUID"], mgr["UUID"])
	}
}

// Every response advertises @odata.context pointing into $metadata, so the
// document has to exist.
func TestMetadataDocumentResolves(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := NewService(&deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}),
		Firmware: firmware.NewController(&config.Config{}),
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

// The System must point back at its Manager: crawlers resolve the managing
// BMC through Links.ManagedBy rather than assuming Managers/1.
func TestSystemLinksManagedBy(t *testing.T) {
	sys := buildSystemResource(context.Background(), power.NewController(config.Hardware{}, config.Power{}))
	if sys.Links == nil || len(sys.Links.ManagedBy) == 0 {
		t.Fatalf("ComputerSystem has no Links.ManagedBy")
	}
	if got := string(sys.Links.ManagedBy[0]); got != managerPath {
		t.Errorf("ManagedBy = %q, want %q", got, managerPath)
	}
}
