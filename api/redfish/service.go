package redfish

import (
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// Service handles Redfish REST API requests.
type Service struct {
	Firmware *firmware.Controller
	Power    *power.Controller

	// Deps is retained for its process-lifetime context: power actions and
	// capsule staging run detached from the request so a Redfish client's
	// short timeout cannot abandon them midway. See deps.ActionContext.
	Deps *deps.Deps
}

// NewService creates a new Redfish service.
func NewService(d *deps.Deps) *Service {
	return &Service{
		Firmware: d.Firmware,
		Power:    d.Power,
		Deps:     d,
	}
}
