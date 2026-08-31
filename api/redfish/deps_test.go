package redfish

import (
	"log/slog"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

// testDeps builds a Deps backed by real, unconfigured controllers (no GPIO
// pins, no image path) so handlers that read them degrade gracefully — the
// same behaviour they have in production before the hardware is configured —
// rather than needing a mock for every test.
func testDeps() *deps.Deps {
	return &deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}, slog.New(slog.DiscardHandler)),
		Firmware: firmware.NewController(&config.Config{}, slog.New(slog.DiscardHandler)),
	}
}
