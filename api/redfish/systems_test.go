package redfish

import (
	"context"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

// The System must point back at its Manager: crawlers resolve the managing
// BMC through Links.ManagedBy rather than assuming Managers/1.
func TestSystemLinksManagedBy(t *testing.T) {
	sys := buildSystemResource(context.Background(), power.NewController(config.Hardware{}, config.Power{}))
	if sys.Links == nil || len(sys.Links.ManagedBy) == 0 {
		t.Fatalf("ComputerSystem has no Links.ManagedBy")
	}
	if got := string(sys.Links.ManagedBy[0]); got != managerPath {
		t.Errorf("ManagedBy = %q, want %q", got, managerPath)
	}
}
