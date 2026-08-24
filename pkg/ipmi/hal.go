package ipmi

import (
	"context"
	"io"

	"github.com/bougou/go-ipmi/pkg/hal"

	"github.com/pi-bmc/nanokvm-app/pkg/bmcsensor"
	"github.com/pi-bmc/nanokvm-app/pkg/serial"
)

// powerController is the slice of power.Controller the chassis HAL needs.
type powerController interface {
	State(ctx context.Context) (bool, error)
	PowerOn(ctx context.Context) error
	PowerOff(ctx context.Context) error
	Reset(ctx context.Context) error
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

func newHAL(root context.Context, pw powerController, broker consoleBroker, sensors sensorSource) *appHAL {
	return &appHAL{
		chassis: &chassisHAL{root: root, power: pw},
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
