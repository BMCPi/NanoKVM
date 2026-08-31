package redfish

// sessions_test.go covers the session-login handshake as bmclib drives
// it. bmclib gates every power/boot/system call on redfishwrapper's
// SessionActive(), which is gofish's APIClient.GetSession() — and that
// returns "client not authenticated" whenever the session-create response
// gave gofish no session URI to remember. Rufio therefore reports a BMC as
// unauthenticated even though the credentials were accepted.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	api := r.Group("/redfish/v1").Use(CheckAuth(h.log, h.d.Auth))
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

// TestCreateSessionUnblocksOnClientDisconnect covers the anti-brute-force
// delay on the failed-credentials branch (see the context/cancellation
// audit's I8 finding): it must wait on c.Request.Context() alongside the
// timer, not call time.Sleep unconditionally, or a client that has already
// gone away still pins the handler goroutine for the full 2s and extends
// shutdown drain.
//
// The comparison is relative rather than against a fixed budget:
// ComparePlainAccount's bcrypt cost is itself highly variable (tens of ms
// unraced, observed 2s+ under -race in a loaded sandbox), so a hardcoded
// "must finish within Nms" threshold would be flaky. Both runs below pay
// that same bcrypt cost; only the cancelled run should skip the extra fixed
// 2s wait, so its duration should land comfortably below the live-context
// baseline regardless of how slow bcrypt is on the machine running the test.
func TestCreateSessionUnblocksOnClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := testHandlers()
	r := gin.New()
	r.POST(sessionsPath, h.CreateSession)

	run := func(ctx context.Context) (time.Duration, *httptest.ResponseRecorder) {
		req := httptest.NewRequest(http.MethodPost, sessionsPath,
			strings.NewReader(`{"UserName":"admin","Password":"wrong"}`)).WithContext(ctx)
		w := httptest.NewRecorder()
		start := time.Now()
		r.ServeHTTP(w, req)
		return time.Since(start), w
	}

	// Baseline: a live request context, so the handler runs the full path —
	// bcrypt compare, then the full 2s anti-brute-force wait.
	baseline, _ := run(context.Background())

	// The client is already gone before the handler even starts.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, w := run(cancelCtx)

	t.Logf("baseline=%v cancelled=%v", baseline, cancelled)
	if want := baseline - time.Second; cancelled >= want {
		t.Fatalf("client-context cancellation did not skip the anti-brute-force delay: "+
			"cancelled=%v, want comfortably below baseline-1s=%v (baseline=%v)", cancelled, want, baseline)
	}

	// On this early-return path the handler must respond nothing extra: no
	// body written, since the client that would have received it is already
	// gone. (ResponseRecorder.Code defaults to 200 whether or not a handler
	// ever calls WriteHeader, so an empty body — not the status — is the
	// signal that nothing was written.)
	if w.Body.Len() != 0 {
		t.Errorf("handler wrote a response body after client-context cancellation: %q", w.Body.String())
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
