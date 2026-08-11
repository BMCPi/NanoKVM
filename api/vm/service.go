package vm

import (
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/video/rtc"
)

type Service struct {
	Power    *power.Controller
	Firmware *firmware.Controller

	// VideoHub is nil when the board has no capture pipeline; the handler
	// answers 503 rather than the route disappearing, so the UI gets a
	// reason instead of a 404. Named for the type rather than the route
	// because the handler below is already Service.Video.
	VideoHub *rtc.Hub
}

func NewService(d *deps.Deps) *Service {
	return &Service{
		Power:    d.Power,
		Firmware: d.Firmware,
		VideoHub: d.Video,
	}
}
