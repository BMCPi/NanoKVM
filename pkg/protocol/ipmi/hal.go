package ipmi

import (
	"context"
	"io"
	"log/slog"

	"github.com/bougou/go-ipmi/pkg/hal"

	"github.com/pi-bmc/nanokvm-app/pkg/device/bmcsensor"
	"github.com/pi-bmc/nanokvm-app/pkg/device/serial"
)

// powerController is the slice of power.Controller the chassis HAL needs.
//
// Restart and CanResetLine back Chassis Control's hard-reset action: Restart
// is the power.reset policy's dispatch entry point (reset-line pulse where
// wired and the policy allows it, else force-off+repower — see
// power.Controller.Restart's doc comment), and CanResetLine lets ColdReset
// reject synchronously, before detaching, the one combination Restart is
// guaranteed never to touch hardware for (policy "line" on an unwired board).
type powerController interface {
	State(ctx context.Context) (bool, error)
	PowerOn(ctx context.Context) error
	PowerOff(ctx context.Context) error
	Reset(ctx context.Context) error
	Restart(ctx context.Context) error
	CanResetLine() bool
}

// consoleBroker is the slice of serial.Broker the SOL console needs.
type consoleBroker interface {
	Connect(id string, output io.Writer) (*serial.Session, error)
	Disconnect(id string)
	Write(p []byte) (int, error)
}

// sensorSource is the slice of bmcsensor.Reader the sensor HAL needs.
type sensorSource interface {
	Read() (bmcsensor.Reading, error)
}

// appHAL is this BMC's hal.HAL: chassis power via the GPIO power controller,
// SOL via the shared serial broker, sensors via the OP-TEE push record, and
// in-memory FRU/SDR stores seeded at startup. Network, GPIO and I2C are
// absent — the handlers answer those NetFns with the proper completion codes.
type appHAL struct {
	chassis *chassisHAL
	console *consoleHAL
	sensors *sensorHAL
	storage *storageHAL
}

// newHAL wires the HAL implementation. resetPolicy is the operator's
// power.reset config (auto|line|cycle) — chassisHAL needs it, not just the
// power controller, to decide synchronously whether ColdReset can reject
// before detaching (see chassisHAL.ColdReset).
func newHAL(root context.Context, pw powerController, resetPolicy string, broker consoleBroker, sensors sensorSource, log *slog.Logger) *appHAL {
	return &appHAL{
		chassis: &chassisHAL{root: root, power: pw, resetPolicy: resetPolicy, log: log},
		console: &consoleHAL{broker: broker},
		sensors: &sensorHAL{source: sensors},
		storage: newStorageHAL(),
	}
}

func (h *appHAL) Chassis() hal.ChassisHAL { return h.chassis }
func (h *appHAL) Sensors() hal.SensorHAL  { return h.sensors }
func (h *appHAL) Storage() hal.StorageHAL { return h.storage }
func (h *appHAL) Console() hal.ConsoleHAL { return h.console }
func (h *appHAL) Network() hal.NetworkHAL { return nil }
func (h *appHAL) GPIO() hal.GPIOHAL       { return nil }
func (h *appHAL) I2C() hal.I2CHAL         { return nil }
func (h *appHAL) Close() error            { return nil }
