package redfish

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
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// bmcUUIDNamespace is a fixed, arbitrary v4 UUID used only as the v5
// namespace. It never changes: changing it would rename every BMC.
var bmcUUIDNamespace = uuid.MustParse("6f4f5f2a-3b0e-5a1d-9c7e-1f2b3c4d5e6f")

var (
	managerUUIDOnce  sync.Once
	managerUUIDValue string
)

// managerUUID returns this BMC's stable UUID, or "" when no stable seed can
// be found (a diskless boot before machine-id exists and with no NIC). An
// empty string is correct in that case: the property is omitempty, and
// advertising an unstable UUID is worse than advertising none, because a
// client would treat each boot as a different device.
func managerUUID() string {
	managerUUIDOnce.Do(func() {
		if seed := bmcIdentitySeed(); seed != "" {
			managerUUIDValue = uuid.NewSHA1(bmcUUIDNamespace, []byte(seed)).String()
		}
	})
	return managerUUIDValue
}

// bmcIdentitySeed returns the most durable identifier available.
func bmcIdentitySeed() string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return "machine-id:" + id
		}
	}

	macs := make([]string, 0, 4)
	for _, nic := range listManagerNICs() {
		if nic.MAC != "" {
			macs = append(macs, strings.ToLower(nic.MAC))
		}
	}
	if len(macs) == 0 {
		return ""
	}
	sort.Strings(macs)
	return "mac:" + macs[0]
}
