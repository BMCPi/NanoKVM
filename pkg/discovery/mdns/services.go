// Package mdns builds the DNS-SD service list a BMC advertises. It is
// deliberately socket-free: the responder that actually publishes these
// records lives elsewhere, so this package can be exercised with plain unit
// tests instead of a live mDNS listener.
package mdns

// DNS-SD service instance types (RFC 6763 §7) for the services table in
// Services below, and the two Inputs.Proto values that select between them.
const (
	svcTypeRedfish = "_redfish._tcp"
	svcTypeHTTP    = "_http._tcp"
	svcTypeHTTPS   = "_https._tcp"
	svcTypeSSH     = "_ssh._tcp"

	protoHTTP  = "http"
	protoHTTPS = "https"
)

// Service is one DNS-SD registration, independent of the library that
// publishes it.
type Service struct {
	Type string // "_redfish._tcp"
	Port int
	Text map[string]string // nil for services with no TXT record
}

// Inputs is everything the service list depends on, passed explicitly so the
// package never reads config.
type Inputs struct {
	Proto          string // "http" or "https"
	HTTPPort       int
	HTTPSPort      int
	RedfishEnabled bool
	SSHEnabled     bool
	SSHPort        int
	UUID           string
}

// Services builds the DNS-SD advertisement list for the given deployment.
// IPMI is deliberately out of scope: it is advertised over its own SLP
// mechanism, not DNS-SD.
func Services(in Inputs) []Service {
	var services []Service

	redfishPort := in.HTTPSPort
	if in.Proto == protoHTTP {
		redfishPort = in.HTTPPort
	}

	if in.RedfishEnabled && redfishPort != 0 {
		text := map[string]string{
			"txtvers":   "1",
			"protovers": "1.0",
			"path":      "/redfish/v1/",
		}
		// Omit uuid rather than publish it empty: a consumer keying on
		// uuid= would otherwise record every BMC without a stable
		// identity under the same key, "".
		if in.UUID != "" {
			text["uuid"] = in.UUID
		}
		services = append(services, Service{
			Type: svcTypeRedfish,
			Port: redfishPort,
			Text: text,
		})
	}

	// The plain HTTP listener runs regardless of proto, so _http._tcp is
	// always advertised when it has a port. _https._tcp is only advertised
	// when proto is https: that is the only time a TLS listener exists at
	// all, so there is no port to advertise otherwise.
	if in.HTTPPort != 0 {
		services = append(services, Service{Type: svcTypeHTTP, Port: in.HTTPPort})
	}
	if in.Proto == protoHTTPS && in.HTTPSPort != 0 {
		services = append(services, Service{Type: svcTypeHTTPS, Port: in.HTTPSPort})
	}

	if in.SSHEnabled && in.SSHPort != 0 {
		services = append(services, Service{Type: svcTypeSSH, Port: in.SSHPort})
	}

	return services
}
