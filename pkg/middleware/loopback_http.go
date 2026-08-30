package middleware

import (
	"net"
	"net/http"
	"strings"
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
