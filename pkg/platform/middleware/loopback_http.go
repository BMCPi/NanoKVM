package middleware

import (
	"net"
	"net/http"
	"strings"
	"time"
)

// NewPlainHTTPServer returns an unstarted server that owns the plain-HTTP
// port while TLS is enabled. Requests sourced from the Redfish host
// interface's subnet are served directly with hostHandler; everything else is
// redirected to HTTPS. The caller owns its lifecycle
// (ListenAndServe/Shutdown), so it participates in the same graceful shutdown
// as the main server.
//
// The host-interface carve-out is a contract, not a convenience: the managed
// host's firmware speaks DSP0270 plain HTTP on the link-local net, and its
// EDK2 Redfish client neither follows redirects nor speaks TLS on that path.
// A blanket redirect therefore severs the entire host sync — identity
// reporting, thermal, and boot-override consumption included — the moment an
// operator enables HTTPS.
//
// The subnet test uses the raw TCP source (req.RemoteAddr), never forwarded
// headers. That is the same trust boundary api/redfish's
// IsHostInterfaceRequest relies on: the nftables isolation in pkg/network
// guarantees 169.254/16 traffic cannot arrive via eth0. An empty or
// unparsable rhiCIDR, or a nil hostHandler, fails closed to redirect-only.
func NewPlainHTTPServer(httpPort, httpsPort, rhiCIDR string, hostHandler http.Handler) *http.Server {
	var rhiNet *net.IPNet
	if _, subnet, err := net.ParseCIDR(rhiCIDR); err == nil {
		rhiNet = subnet
	}

	return &http.Server{
		Addr: httpPort,
		// Bound how long a client may dribble out its request headers, so an
		// idle half-open connection cannot pin a server goroutine (Slowloris).
		// Deliberately generous: the host firmware's DSP0270 client on the
		// link-local net is slow, and this listener serves it directly.
		ReadHeaderTimeout: 30 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if hostHandler != nil && rhiNet != nil && remoteInSubnet(req.RemoteAddr, rhiNet) {
				hostHandler.ServeHTTP(w, req)
				return
			}

			host := req.Host
			if strings.Contains(host, httpPort) {
				host = strings.Split(host, httpPort)[0]
			}

			targetURL := "https://" + host + req.URL.String()
			if httpsPort != ":443" {
				targetURL = "https://" + host + httpsPort + req.URL.String()
			}

			// The only request-controlled part of targetURL is req.Host — the
			// name the client itself used to reach this listener — plus the
			// path it asked for. Nothing a third party can put in a link
			// (query string, header a browser will forward cross-origin)
			// reaches the authority, so this cannot be aimed at a victim: a
			// caller can only redirect itself to the host it already typed.
			// The host cannot be pinned to an allowlist either — the BMC is
			// reached by DHCP address, mDNS name and whatever DNS name the
			// operator assigned, none of them known here.
			//nolint:gosec // G710: authority comes from the caller's own Host header, not from any third-party-supplied URL
			http.Redirect(w, req, targetURL, http.StatusTemporaryRedirect)
		}),
	}
}

// remoteInSubnet reports whether addr's IP half lies inside subnet.
func remoteInSubnet(addr string, subnet *net.IPNet) bool {
	hostPart, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	ip := net.ParseIP(hostPart)
	if ip == nil {
		return false
	}

	return subnet.Contains(ip)
}
