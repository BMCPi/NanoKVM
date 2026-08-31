// Package identity derives this BMC's stable identity.
//
// It lives under pkg rather than api/redfish because pkg/discovery publishes
// the same UUID over SSDP and DNS-SD, and a pkg may not import api — the
// dependency only ever runs the other way.
package identity

// identity.go derives this BMC's stable identity.
//
// Inventory tools key a discovered endpoint on a UUID and expect it to name
// the same device tomorrow. A random UUID per boot would make every scan
// register a new node, so the value is derived deterministically from
// something the hardware owns.
//
// The source, in order of preference:
//
//  1. /etc/machine-id — persistent, unique, and already the system's identity
//     of record. Present on this device once first boot has populated it.
//  2. The lowest permanent MAC across the BMC's own NICs. Ordering by value
//     rather than by interface name keeps the answer stable when the kernel
//     enumerates interfaces differently across boots.
//
// Either way the raw value is hashed into a v5 UUID under a fixed namespace,
// so the MAC is not published verbatim in a second place and the result is a
// well-formed UUID whatever the input length.

import (
	"net"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// bmcUUIDNamespace is a fixed, arbitrary v4 UUID used only as the v5
// namespace. It never changes: changing it would rename every BMC.
var bmcUUIDNamespace = uuid.MustParse("6f4f5f2a-3b0e-5a1d-9c7e-1f2b3c4d5e6f")

// RedfishProtocolVersion is the value the service root reports as
// RedfishVersion. It is the *protocol* version from DSP0266's Protocol
// Version clause — not the ServiceRoot schema version, which is a separate
// number (odataTypeServiceRoot in api/redfish).
//
// 1.13.0 is the lowest protocol version consistent with what this BMC
// actually serves: odataTypeComputerSystem is pinned to v1_13_0, and
// BootProgress — which the host firmware PATCHes and systems.go publishes —
// is that version's addition. Clients gate on this. bmclib, the library
// behind Tinkerbell's Rufio, refuses to read BootProgress from a service
// reporting less than 1.13.0, so the "1.0.0" this replaced was silently
// switching off a feature we implement.
//
// Protocol minor versions are additive, so under-claiming costs features
// while over-claiming promises behaviour we do not have: raise this only to
// match a capability actually added here. Not implementing
// ProtocolFeaturesSupported (protocol 1.6+) is not a reason to claim less —
// that property is optional, and its absence correctly means "no query
// parameters supported".
//
// It lives here, rather than in api/redfish where it originated, because
// pkg/discovery advertises this same version over SSDP/DNS-SD and a pkg may
// not import api.
const RedfishProtocolVersion = "1.13.0"

var (
	bmcUUIDOnce  sync.Once
	bmcUUIDValue string
)

// BMCUUID returns this BMC's stable UUID, or "" when no stable seed can be
// found (a diskless boot before machine-id exists and with no NIC). An empty
// string is correct in that case: callers treat it as omitempty, and
// advertising an unstable UUID is worse than advertising none, because a
// client would treat each boot as a different device.
func BMCUUID() string {
	bmcUUIDOnce.Do(func() {
		if seed := bmcIdentitySeed(); seed != "" {
			bmcUUIDValue = uuid.NewSHA1(bmcUUIDNamespace, []byte(seed)).String()
		}
	})
	return bmcUUIDValue
}

// bmcIdentitySeed returns the most durable identifier available.
func bmcIdentitySeed() string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return "machine-id:" + id
		}
	}

	if mac := lowestMAC(); mac != "" {
		return "mac:" + mac
	}
	return ""
}

// lowestMAC returns the lowest permanent MAC across the BMC's own NICs,
// lowercased. Ordering by value rather than by interface name keeps the
// answer stable when the kernel enumerates interfaces differently across
// boots.
func lowestMAC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	macs := make([]string, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if mac := ifi.HardwareAddr.String(); mac != "" {
			macs = append(macs, strings.ToLower(mac))
		}
	}
	if len(macs) == 0 {
		return ""
	}
	sort.Strings(macs)
	return macs[0]
}
