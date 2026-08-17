package vm

import (
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/hid"
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

	// HIDGadget is the USB keyboard/mouse gadget driver, nil when the gadget is
	// not configured on this board. Same contract as VideoHub: the handler
	// answers 503 so the UI can say why instead of hitting a 404, and it is
	// named for the type because the handler below is already Service.HID.
	HIDGadget *hid.Controller

	// Conf is the live config, for the stored keyboard macros.
	Conf *config.Config
}

func NewService(d *deps.Deps) *Service {
	return &Service{
		Power:     d.Power,
		Firmware:  d.Firmware,
		VideoHub:  d.Video,
		HIDGadget: d.HID,
		Conf:      d.Config,
	}
}
