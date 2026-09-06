package ipmi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/bougou/go-ipmi/pkg/hal"
	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/bougou/go-ipmi/pkg/types"
)

// Sensor numbers, stable across releases: they are the key clients cache
// from the SDR.
const (
	sensorNumSoCTemp = 1
	sensorNumFanDuty = 2
)

// sensorHAL serves the two readings the managed host's registered
// hostsensor.Source carries: the die temperature (°C, M=1 linear) and the
// commanded fan duty (percent). source is nil on a board with no such
// channel (see pkg/device/hostsensor), which this HAL treats as "no sensors"
// rather than panicking on.
type sensorHAL struct {
	source sensorSource
}

func (s *sensorHAL) List(_ context.Context) ([]hal.SensorDescriptor, error) {
	if s.source == nil {
		return nil, nil
	}
	return []hal.SensorDescriptor{
		{ID: sensorNumSoCTemp, Type: uint8(types.SensorTypeTemperature), Name: "SoC Temp"},
		{ID: sensorNumFanDuty, Type: uint8(types.SensorTypeFan), Name: "Fan Duty"},
	}, nil
}

func (s *sensorHAL) ReadRaw(_ context.Context, sensorID uint8) (uint8, error) {
	switch sensorID {
	case sensorNumSoCTemp, sensorNumFanDuty:
	default:
		return 0, hal.ErrNotFound
	}
	if s.source == nil {
		return 0, hal.ErrNotFound
	}

	reading, ok := s.source.Latest()
	if !ok {
		return 0, fmt.Errorf("no sensor reading available")
	}
	if reading.Stale {
		return 0, fmt.Errorf("sensor record stale (host stopped pushing)")
	}

	switch sensorID {
	case sensorNumSoCTemp:
		if !reading.TempValid {
			return 0, fmt.Errorf("temperature not valid in current record")
		}
		c := int(math.Round(reading.TempC))
		if c < 0 {
			c = 0
		}
		if c > 255 {
			c = 255
		}
		return uint8(c), nil
	default: // sensorNumFanDuty
		if !reading.FanValid {
			return 0, fmt.Errorf("fan state not valid in current record")
		}
		return uint8(reading.FanDutyPct), nil
	}
}

// registerSensorHandlers adds the Sensor/Event NetFn commands the framework
// does not ship. Only Get Sensor Reading is needed for `ipmitool sensor` /
// `sdr` output: the analog conversion lives in the seeded Full Sensor SDRs.
func registerSensorHandlers(reg *handlers.Registry, s *sensorHAL) {
	reg.RegisterFunc(
		types.Command{ID: 0x2D, NetFn: types.NetFnSensorEventRequest, Name: "Get Sensor Reading"},
		func(ctx context.Context, _ *handlers.HandlerContext, req []byte) ([]byte, types.CompletionCode, error) {
			if len(req) < 1 {
				return nil, types.CodeRequestDataTruncated, nil
			}
			raw, err := s.ReadRaw(ctx, req[0])
			if errors.Is(err, hal.ErrNotFound) {
				return nil, types.CodeRequestedDataNotPresent, nil
			}
			// Byte 2: [7] event messages enabled, [6] scanning enabled,
			// [5] reading unavailable. Byte 3: threshold comparison status
			// (none asserted; the SDRs define no thresholds).
			if err != nil {
				return []byte{0x00, 0xC0 | 0x20, 0x00}, types.CodeOK, nil
			}
			return []byte{raw, 0xC0, 0x00}, types.CodeOK, nil
		})
}
