package redfish

// sessions_test.go covers the session-login handshake as bmclib drives
// it. bmclib gates every power/boot/system call on redfishwrapper's
// SessionActive(), which is gofish's APIClient.GetSession() — and that
// returns "client not authenticated" whenever the session-create response
// gave gofish no session URI to remember. Rufio therefore reports a BMC as
// unauthenticated even though the credentials were accepted.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish"
)

// authTestServer mounts the real session routes plus one protected resource
// behind the real CheckAuth middleware.
func authTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	h := testHandlers()
	r := gin.New()

	r.GET(ServiceRootPath, svc.GetServiceRoot)
	r.GET(strings.TrimSuffix(ServiceRootPath, "/"), svc.GetServiceRoot)
	r.GET(sessionServicePath, h.GetSessionService)
	r.POST(sessionsPath, h.CreateSession)

	api := r.Group("/redfish/v1").Use(CheckAuth())
	{
		api.GET("/Systems", h.GetSystemCollection)
		api.GET("/Systems/1", h.GetSystem)
		api.GET("/SessionService/Sessions", h.GetSessionCollection)
		api.GET("/SessionService/Sessions/:id", h.GetSession)
		api.DELETE("/SessionService/Sessions/:id", h.DeleteSession)
	}

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// TestSessionCreateReturnsLocation asserts the wire contract: a 201 from
// POST Sessions must carry both X-Auth-Token and Location. gofish reads the
// session URI out of Location and nowhere else.
func TestSessionCreateReturnsLocation(t *testing.T) {
	ts := authTestServer(t)

	resp, err := http.Post(ts.URL+sessionsPath, "application/json",
		strings.NewReader(`{"UserName":"admin","Password":"admin"}`))
	if err != nil {
		t.Fatalf("POST Sessions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("X-Auth-Token") == "" {
		t.Error("no X-Auth-Token header")
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Error("no Location header: gofish stores the session URI from Location, " +
			"so bmclib's SessionActive() reports 'client not authenticated'")
	} else if !strings.HasPrefix(loc, sessionsPath+"/") {
		t.Errorf("Location = %q, want a %s/<id> URI", loc, sessionsPath)
	}
}

// TestGofishSessionAuth is the end-to-end reproduction of the Rufio failure.
func TestGofishSessionAuth(t *testing.T) {
	ts := authTestServer(t)

	client, err := gofish.Connect(gofish.ClientConfig{
		Endpoint: ts.URL,
		Username: "admin",
		Password: "admin",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("gofish.Connect (session login): %v", err)
	}
	defer client.Logout()

	// This exact call is bmclib redfishwrapper.SessionActive(), which gates
	// PowerStateGet, PowerSet, BootDeviceSet and Compatible().
	if _, err := client.GetSession(); err != nil {
		t.Fatalf("bmclib SessionActive() equivalent failed: %v", err)
	}

	// And the token must actually open a protected resource.
	systems, err := client.GetService().Systems()
	if err != nil {
		t.Fatalf("authenticated GET Systems: %v", err)
	}
	if len(systems) != 1 {
		t.Fatalf("discovered %d systems, want 1", len(systems))
	}
}

// TestGofishSessionGetAndDelete covers the session URI gofish is handed:
// it must be GETtable (some clients re-read it) and DELETEable on Logout.
func TestGofishSessionGetAndDelete(t *testing.T) {
	ts := authTestServer(t)

	client, err := gofish.Connect(gofish.ClientConfig{
		Endpoint: ts.URL,
		Username: "admin",
		Password: "admin",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("gofish.Connect: %v", err)
	}

	sess, err := client.GetSession()
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	resp, err := client.Get(sess.ID)
	if err != nil {
		t.Errorf("GET %s: %v", sess.ID, err)
	} else {
		resp.Body.Close()
	}
	if err := client.GetService().DeleteSession(sess.ID); err != nil {
		t.Errorf("DELETE %s: %v", sess.ID, err)
	}
}
