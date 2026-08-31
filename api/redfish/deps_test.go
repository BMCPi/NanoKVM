package redfish

import (
	"context"
	"log/slog"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// testDeps builds a Deps backed by real, unconfigured controllers (no GPIO
// pins, no image path) so handlers that read them degrade gracefully — the
// same behaviour they have in production before the hardware is configured —
// rather than needing a mock for every test.
func testDeps() *deps.Deps {
	return &deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}, slog.New(slog.DiscardHandler)),
		Firmware: firmware.NewController(&config.Config{}, slog.New(slog.DiscardHandler)),
		Auth:     auth.NewService(context.Background(), slog.New(slog.DiscardHandler)),
	}
}

// testHandlers builds a *handlers over testDeps() with a discard logger, for
// the routes this package's log-touched files (sensors.go, serial_interfaces.go,
// sessions.go, systems.go, update_service.go, virtual_media.go) now serve —
// the rest of the package's routes still come off NewService.
func testHandlers() *handlers {
	return &handlers{d: testDeps(), log: slog.New(slog.DiscardHandler)}
}
