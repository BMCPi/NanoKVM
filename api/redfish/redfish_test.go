package redfish

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

// The service root is registered at both "/redfish/v1/" and "/redfish/v1"
// while the protected group also owns "/redfish/v1/..." children. gin builds
// a radix tree over all of them, so this asserts the real router — not an
// isolated pair of routes — accepts the registration and answers both forms.
func TestRedfishRouterServesBothServiceRootForms(t *testing.T) {
	gin.SetMode(gin.TestMode)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Register panicked building the route tree: %v", r)
		}
	}()

	r := gin.New()
	Register(r, testDeps())

	// schemas.DefaultServiceRoot is what gofish requests on Login; the bare
	// form is what pre-migration clients use. Both must answer 200 directly,
	// without relying on gin's 301 redirect.
	for _, path := range []string{schemas.DefaultServiceRoot, "/redfish/v1", "/redfish"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200 (body: %s)", path, w.Code, w.Body.String())
			}
		})
	}
}

// The protected children must still route correctly alongside the new
// trailing-slash root — 401 from CheckAuth proves the route matched rather
// than 404'ing into the root handler.
func TestRedfishRouterProtectedRoutesStillMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Register(r, testDeps())

	for _, path := range []string{
		"/redfish/v1/Systems",
		"/redfish/v1/Systems/1",
		"/redfish/v1/Managers/1",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code == http.StatusNotFound {
				t.Errorf("GET %s = 404; the route stopped matching", path)
			}
		})
	}
}
