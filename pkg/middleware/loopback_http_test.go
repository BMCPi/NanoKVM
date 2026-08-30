package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// markerHandler stands in for the real gin engine: anything it serves is
// proof the request was passed through rather than redirected.
var markerHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("served-plain"))
})

func doPlainHTTP(t *testing.T, srv *http.Server, remoteAddr, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	// Path-only target plus explicit Host mirrors what a server-received
	// request looks like: req.URL carries just the path, never the scheme.
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

// LAN clients keep the existing behavior: everything on the plain port is
// redirected to HTTPS, with the port suffix only when HTTPS is non-standard.
func TestPlainHTTPRedirectsLANRequests(t *testing.T) {
	cases := []struct {
		name      string
		httpsPort string
		host      string
		path      string
		want      string
	}{
		{"default https port", ":443", "bmc.example", "/redfish/v1/Systems/1", "https://bmc.example/redfish/v1/Systems/1"},
		{"custom https port", ":8443", "bmc.example", "/", "https://bmc.example:8443/"},
		{"host with http port trimmed", ":443", "bmc.example:80", "/ui", "https://bmc.example/ui"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewPlainHTTPServer(":80", tc.httpsPort, "169.254.10.1/16", markerHandler)
			rec := doPlainHTTP(t, srv, "10.1.40.31:52044", tc.host, tc.path)
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want 307", rec.Code)
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

// The managed host's firmware speaks DSP0270 plain HTTP on the RHI subnet and
// cannot follow a redirect; requests sourced there must reach the real
// handler even while TLS is enabled.
func TestPlainHTTPServesHostInterfaceDirectly(t *testing.T) {
	srv := NewPlainHTTPServer(":80", ":443", "169.254.10.1/16", markerHandler)
	rec := doPlainHTTP(t, srv, "169.254.10.2:49152", "169.254.10.1", "/redfish/v1/Systems/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (served, not redirected)", rec.Code)
	}
	if rec.Body.String() != "served-plain" {
		t.Fatalf("body = %q, want the passthrough handler's body", rec.Body.String())
	}
}

// Anything that stops the subnet check from being trustworthy fails closed to
// redirect-only: no CIDR, a bad CIDR, no handler, or an unparsable source.
func TestPlainHTTPFailsClosedToRedirect(t *testing.T) {
	cases := []struct {
		name       string
		rhiCIDR    string
		handler    http.Handler
		remoteAddr string
	}{
		{"empty CIDR", "", markerHandler, "169.254.10.2:49152"},
		{"invalid CIDR", "not-a-cidr", markerHandler, "169.254.10.2:49152"},
		{"nil handler", "169.254.10.1/16", nil, "169.254.10.2:49152"},
		{"unparsable remote", "169.254.10.1/16", markerHandler, "garbage"},
		{"LAN source inside nothing", "169.254.10.1/16", markerHandler, "10.0.0.5:1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewPlainHTTPServer(":80", ":443", tc.rhiCIDR, tc.handler)
			rec := doPlainHTTP(t, srv, tc.remoteAddr, "bmc.example", "/")
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want 307 (fail closed)", rec.Code)
			}
		})
	}
}
