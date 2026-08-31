package redfish

import "github.com/pi-bmc/nanokvm-app/pkg/protocol/identity"

// managerUUID is this BMC's stable identity. It lives in pkg/identity because
// pkg/discovery publishes the same value over SSDP and DNS-SD, and a pkg may
// not import api.
func managerUUID() string { return identity.BMCUUID() }
