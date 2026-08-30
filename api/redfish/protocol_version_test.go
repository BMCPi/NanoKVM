package redfish

// protocol_version_test.go pins the RedfishVersion the service root reports.
//
// RedfishVersion is the *protocol* version (DSP0266's Protocol Version
// clause), not the ServiceRoot schema version, and clients gate features on
// it. bmclib — the library behind Tinkerbell's Rufio — refuses to read
// BootProgress from any service reporting less than 1.13.0:
//
//	// The redfish standard adopts the BootProgress object in 1.13.0. ...
//	if !redfishVersionMeetsOrExceeds(c.client.Service.RedfishVersion, 1, 13, 0) {
//	        return nil, fmt.Errorf("%w: %s", ErrRedfishVersionIncompatible, ...)
//	}
//
// We do serve BootProgress: the host firmware PATCHes BootProgress.LastState
// and systems.go publishes it. Reporting a lower version silently disables a
// feature this BMC implements, which is what "1.0.0" did.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// serviceRootRedfishVersion GETs the service root and returns the
// RedfishVersion property as it appears on the wire.
func serviceRootRedfishVersion(t *testing.T) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	r := gin.New()
	r.GET(ServiceRootPath, svc.GetServiceRoot)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, ServiceRootPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", ServiceRootPath, w.Code)
	}

	var body struct {
		RedfishVersion string `json:"RedfishVersion"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal service root: %v", err)
	}
	return body.RedfishVersion
}

// redfishVersionMeetsOrExceeds is bmclib's comparison, reproduced verbatim
// from internal/redfishwrapper/client.go so this test fails for exactly the
// reason a real client would reject us. Note it requires precisely three
// dot-separated integers — the string's shape is load-bearing, not just its
// value.
func redfishVersionMeetsOrExceeds(version string, major, minor, patch int) bool {
	if version == "" {
		return false
	}

	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}

	var rfVer []int64
	for _, part := range parts {
		ver, err := strconv.ParseInt(part, 10, 32)
		if err != nil {
			return false
		}
		rfVer = append(rfVer, ver)
	}

	if rfVer[0] < int64(major) {
		return false
	}
	if rfVer[1] < int64(minor) {
		return false
	}
	return rfVer[2] >= int64(patch)
}

// TestRedfishVersionPassesBootProgressGate is the regression guard: lowering
// RedfishVersion below 1.13.0 turns BootProgress back off for Rufio.
func TestRedfishVersionPassesBootProgressGate(t *testing.T) {
	got := serviceRootRedfishVersion(t)

	if !redfishVersionMeetsOrExceeds(got, 1, 13, 0) {
		t.Errorf("RedfishVersion = %q; bmclib requires >= 1.13.0 to read the "+
			"BootProgress this BMC publishes", got)
	}
}

// The version must parse as major.minor.errata. bmclib treats any other shape
// as "older than everything", so a value like "1.13" fails every gate while
// looking newer than the one it replaced.
func TestRedfishVersionIsThreeNumericParts(t *testing.T) {
	got := serviceRootRedfishVersion(t)

	parts := strings.Split(got, ".")
	if len(parts) != 3 {
		t.Fatalf("RedfishVersion = %q, want three dot-separated parts", got)
	}
	for _, p := range parts {
		if _, err := strconv.ParseInt(p, 10, 32); err != nil {
			t.Errorf("RedfishVersion = %q: part %q is not an integer", got, p)
		}
	}
}
