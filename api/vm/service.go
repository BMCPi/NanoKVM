package vm

import (
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

type Service struct {
	Power    *power.Controller
	Firmware *firmware.Controller
}

func NewService(d *deps.Deps) *Service {
	return &Service{
		Power:    d.Power,
		Firmware: d.Firmware,
	}
}
