package ui

// fragments_settings_test.go guards the mDNS settings form against reading
// and writing different config blocks — see
// TestPatchMDNSWritesTheBlockDiscoveryReadsFrom below.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
)

// settingsRouter mounts the settings fragment routes without the auth
// middleware, mirroring biosRouter in fragments_bios_test.go.
func settingsRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	d := &deps.Deps{}
	r := gin.New()
	r.Use(deps.Middleware(d))
	settingsFragmentRoutes(r.Group("/ui"), d)
	return r
}

// TestPatchMDNSWritesTheBlockDiscoveryReadsFrom is the regression test for a
// bug where the settings form read/wrote config.GetInstance().MDNS — a
// legacy field kept only as migrateDiscovery's landing spot (see
// pkg/config/default.go) — while pkg/discovery starts only from
// Discovery.MDNS. The form looked like it worked (toast, re-rendered
// values) but discovery.Restart() picked up nothing, and the legacy values
// were then discarded for good on the next config.Save()/reload since a
// written discovery: key makes migrateDiscovery skip the legacy block.
//
// Writing that field is now worse still, not better: migrateDiscovery
// clears it on load and the rewrite drops the key, so a write there both
// goes nowhere and resurrects a spelling the migration exists to remove.
//
// This fails if patchMDNS (the write) and networkModel (the read) ever
// disagree again about which struct field is authoritative.
func TestPatchMDNSWritesTheBlockDiscoveryReadsFrom(t *testing.T) {
	cfg := config.GetInstance()
	origLegacy, origDiscovery, origRedfish := cfg.MDNS, cfg.Discovery, cfg.Redfish
	t.Cleanup(func() {
		cfg.MDNS, cfg.Discovery, cfg.Redfish = origLegacy, origDiscovery, origRedfish
	})

	// Seed the legacy block with values distinct from the request below, so
	// a write that lands there instead of Discovery.MDNS is caught by
	// asserting it stayed untouched. SSDP/Redfish are forced off so
	// discovery.Restart() (which patchMDNS calls unconditionally) takes its
	// documented no-socket path regardless of the mDNS enabled bit this test
	// submits — this test is about which config field gets written, not
	// about exercising a live responder.
	cfg.MDNS = &config.MDNS{Enabled: true, Hostname: "legacy-should-not-change", Interface: "legacy0"}
	cfg.Discovery.MDNS = config.MDNS{Enabled: true, Hostname: "old", Interface: "eth0"}
	cfg.Discovery.SSDP = config.SSDP{Enabled: false}
	cfg.Redfish.Enabled = false

	r := settingsRouter(t)

	// "enabled" is deliberately omitted so the write sets Discovery.MDNS.Enabled
	// to false, the opposite of both its seeded value and the legacy block's —
	// proving the boolean actually moved through this write, not just the
	// string fields.
	form := strings.NewReader("hostname=newname&interface=eth1&ipv4=on&ipv6=on")
	req := httptest.NewRequest(http.MethodPatch, "/ui/settings/mdns", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	got := config.GetInstance().Discovery.MDNS
	if got.Enabled || got.Hostname != "newname" || got.Interface != "eth1" || !got.IPv4 || !got.IPv6 {
		t.Errorf("Discovery.MDNS after patch = %+v, want the submitted values (Enabled=false)", got)
	}

	if legacy := config.GetInstance().MDNS; legacy == nil || legacy.Hostname != "legacy-should-not-change" {
		t.Errorf("legacy MDNS block was mutated: %+v (patchMDNS must write Discovery.MDNS only)", legacy)
	}

	// networkModel is what the re-rendered form (and the next GET) reads
	// from; it must agree with what was just written, not the legacy block.
	m := networkModel()
	if m.MDNSHostname != "newname" || m.MDNSEnabled {
		t.Errorf("networkModel() = %+v, want it to reflect the write via Discovery.MDNS", m)
	}
}
