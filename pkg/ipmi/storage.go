package ipmi

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/bougou/go-ipmi/pkg/hal"
	"github.com/bougou/go-ipmi/pkg/types"
)

// storageHAL is in-memory FRU/SDR storage. The documents are small, seeded at
// startup, and rebuilt on every boot, so nothing needs to touch the flash.
type storageHAL struct {
	fru *fruStore
	sdr *sdrStore
}

func newStorageHAL() *storageHAL {
	return &storageHAL{
		fru: &fruStore{memStore[uint8]{blobs: make(map[uint8][]byte)}},
		sdr: &sdrStore{memStore[uint16]{blobs: make(map[uint16][]byte)}},
	}
}

// fruStore and sdrStore name the ID-listing method each hal interface wants.
type (
	fruStore struct{ memStore[uint8] }
	sdrStore struct{ memStore[uint16] }
)

func (f *fruStore) DeviceIDs(ctx context.Context) ([]uint8, error)  { return f.ids(ctx) }
func (s *sdrStore) RecordIDs(ctx context.Context) ([]uint16, error) { return s.ids(ctx) }

func (s *storageHAL) FRU() hal.FRUStore { return s.fru }
func (s *storageHAL) SDR() hal.SDRStore { return s.sdr }

// memStore is a mutex-guarded blob map satisfying both hal.FRUStore (uint8
// keys) and hal.SDRStore (uint16 keys).
type memStore[K uint8 | uint16] struct {
	mu    sync.Mutex
	blobs map[K][]byte
}

func (m *memStore[K]) Read(_ context.Context, id K) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[id]
	if !ok {
		return nil, hal.ErrNotFound
	}
	return slices.Clone(b), nil
}

func (m *memStore[K]) Write(_ context.Context, id K, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[id] = slices.Clone(data)
	return nil
}

func (m *memStore[K]) Delete(_ context.Context, id K) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[id]; !ok {
		return hal.ErrNotFound
	}
	delete(m.blobs, id)
	return nil
}

func (m *memStore[K]) ids(_ context.Context) ([]K, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]K, 0, len(m.blobs))
	for id := range m.blobs {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

// SDR record IDs. Like the sensor numbers these are part of what clients
// cache, so they stay stable.
const (
	sdrRecMCLocator uint16 = 1
	sdrRecSoCTemp   uint16 = 2
	sdrRecFanDuty   uint16 = 3
)

// seedStorage populates the FRU and SDR stores with this board's documents.
func seedStorage(ctx context.Context, s *storageHAL) error {
	fruData, err := types.PackFRU(types.FRUPackConfig{
		Product: &types.FRUPackProduct{
			Manufacturer: "pi-bmc",
			Name:         "NanoKVM BMC",
			Version:      "2.0",
		},
	})
	if err != nil {
		return fmt.Errorf("pack fru: %w", err)
	}
	if err := s.fru.Write(ctx, 0, fruData); err != nil {
		return err
	}

	if err := s.sdr.Write(ctx, sdrRecMCLocator, types.PackMCLocator(types.MCLocatorPackOpts{
		RecordID: sdrRecMCLocator,
		// FRU inventory + SDR repository + sensor device.
		DeviceSupport: 0x0B,
		Name:          "NanoKVM BMC",
	})); err != nil {
		return err
	}

	// SoC die temperature: raw byte is whole °C (M=1, linear, unsigned).
	temp := fullSensorSDR(sensorNumSoCTemp, "SoC Temp", types.SensorTypeTemperature,
		0x03 /* processor */, types.SensorUnit{
			AnalogDataFormat: types.SensorAnalogUnitFormat_Unsigned,
			BaseUnit:         types.SensorUnitType_DegreesC,
		}, 0xFF)
	if err := s.sdr.Write(ctx, sdrRecSoCTemp, temp.Pack(sdrRecSoCTemp)); err != nil {
		return err
	}

	// Commanded fan duty: raw byte is whole percent (M=1, linear, unsigned).
	fan := fullSensorSDR(sensorNumFanDuty, "Fan Duty", types.SensorTypeFan,
		0x1D /* fan device */, types.SensorUnit{
			AnalogDataFormat: types.SensorAnalogUnitFormat_Unsigned,
			Percentage:       true,
		}, 100)
	return s.sdr.Write(ctx, sdrRecFanDuty, fan.Pack(sdrRecFanDuty))
}

// fullSensorSDR builds a threshold-class Full Sensor record (Type 01h) for a
// linear M=1 analog sensor with no thresholds defined.
func fullSensorSDR(num uint8, name string, sensorType types.SensorType, entity types.EntityID, unit types.SensorUnit, maxRaw uint8) *types.SDRFull {
	return &types.SDRFull{
		GeneratorID:            types.GeneratorID(0x0020), // this BMC
		SensorNumber:           types.SensorNumber(num),
		SensorEntityID:         entity,
		SensorEntityInstance:   1,
		SensorType:             sensorType,
		SensorEventReadingType: types.EventReadingType(0x01), // threshold-class
		SensorUnit:             unit,
		LinearizationFunc:      types.LinearizationFunc_Linear,
		ReadingFactors:         types.ReadingFactors{M: 1},
		SensorMaxReadingRaw:    maxRaw,
		SensorMinReadingRaw:    0,
		IDStringTypeLength:     types.TypeLength(0xC0 | len(name)), //nolint:gosec // name is always one of this file's short literals ("SoC Temp", "Fan Duty"), well under the SDR ID-string length field's 6 bits
		IDStringBytes:          []byte(name),
	}
}
