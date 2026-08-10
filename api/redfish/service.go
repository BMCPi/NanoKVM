package redfish

import (
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

// Service handles Redfish REST API requests.
type Service struct {
	Firmware *firmware.Controller
	Power    *power.Controller
}

// NewService creates a new Redfish service.
func NewService(d *deps.Deps) *Service {
	return &Service{
		Firmware: d.Firmware,
		Power:    d.Power,
	}
}
