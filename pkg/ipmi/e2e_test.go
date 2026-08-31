package ipmi

// e2e_test drives the assembled server over real loopback UDP with the
// go-ipmi *client* — the same stack ipmitool speaks — so what is verified is
// the wire behavior: RMCP+ session setup, chassis control, boot options,
// sensors, SDR/FRU, and the OEM command.

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/device/bmcsensor"
	"github.com/pi-bmc/nanokvm-app/pkg/device/serial"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// --- fakes -------------------------------------------------------------------

type fakePower struct {
	mu    sync.Mutex
	on    bool
	calls chan string
}

func newFakePower(on bool) *fakePower {
	return &fakePower{on: on, calls: make(chan string, 16)}
}

func (f *fakePower) record(what string) {
	select {
	case f.calls <- what:
	default:
	}
}

func (f *fakePower) State(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on, nil
}

func (f *fakePower) PowerOn(_ context.Context) error {
	f.mu.Lock()
	f.on = true
	f.mu.Unlock()
	f.record("on")
	return nil
}

func (f *fakePower) PowerOff(_ context.Context) error {
	f.mu.Lock()
	f.on = false
	f.mu.Unlock()
	f.record("off")
	return nil
}

func (f *fakePower) Reset(_ context.Context) error {
	f.record("reset")
	return nil
}

func (f *fakePower) waitCall(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-f.calls:
		if got != want {
			t.Fatalf("power call = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("power call %q never happened", want)
	}
}

type fakeFirmware struct{ status firmware.Status }

func (f fakeFirmware) GetStatus() firmware.Status { return f.status }

type fakeBroker struct {
	mu        sync.Mutex
	out       io.Writer
	keystroke bytes.Buffer
}

func (b *fakeBroker) Connect(_ string, output io.Writer) (*serial.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.out = output
	return nil, nil
}

func (b *fakeBroker) Disconnect(_ string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.out = nil
}

func (b *fakeBroker) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.keystroke.Write(p)
}

type fakeSensors struct{ reading bmcsensor.Reading }

func (f fakeSensors) Read() (bmcsensor.Reading, error) { return f.reading, nil }

// sensorReading builds a Reading through the real wire parser, since the
// fan-block validity flag is not settable from outside the package.
func sensorReading(t *testing.T, milliC int32, dutyPct uint8) bmcsensor.Reading {
	t.Helper()
	b := make([]byte, bmcsensor.RecordSize)
	binary.LittleEndian.PutUint32(b[0:4], bmcsensor.RecordMagic)
	binary.LittleEndian.PutUint16(b[4:6], bmcsensor.RecordVersion)
	binary.LittleEndian.PutUint16(b[6:8], bmcsensor.RecordSize)
	binary.LittleEndian.PutUint32(b[8:12], 7) // seq
	binary.LittleEndian.PutUint32(b[12:16], uint32(milliC))
	binary.LittleEndian.PutUint32(b[20:24], bmcsensor.StatusTempValid)
	b[24] = 2 // fan level
	b[25] = 4
	b[26] = dutyPct
	b[27] = bmcsensor.FanValidFlag
	binary.LittleEndian.PutUint32(b[bmcsensor.RecordSize-4:], crc32.ChecksumIEEE(b[:bmcsensor.RecordSize-4]))

	rec, err := bmcsensor.ParseRecord(b)
	if err != nil {
		t.Fatalf("build sensor record: %v", err)
	}
	return bmcsensor.Reading{Record: rec, At: time.Now()}
}

// --- the test ----------------------------------------------------------------

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fp := newFakePower(true)
	srv, err := startServer(ctx, deps{
		port:     0,
		username: "admin",
		password: "admin",
		power:    fp,
		firmware: fakeFirmware{status: firmware.Status{VolumeReady: true, Staging: true, VolumeSize: 48 << 20}},
		broker:   &fakeBroker{},
		sensors:  fakeSensors{reading: sensorReading(t, 55400, 49)},
		log:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	port := srv.Addr().(*net.UDPAddr).Port
	cl, err := client.NewClient("127.0.0.1", port, "admin", "admin")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := cl.Connect(ctx); err != nil {
		t.Fatalf("lanplus connect: %v", err)
	}
	defer cl.Close(ctx)

	t.Run("device id", func(t *testing.T) {
		resp, err := cl.GetDeviceID(ctx)
		if err != nil {
			t.Fatalf("get device id: %v", err)
		}
		if resp.DeviceID != 0x20 {
			t.Errorf("DeviceID = %#x, want 0x20", resp.DeviceID)
		}
	})

	t.Run("chassis status", func(t *testing.T) {
		resp, err := cl.GetChassisStatus(ctx)
		if err != nil {
			t.Fatalf("get chassis status: %v", err)
		}
		if !resp.PowerIsOn {
			t.Errorf("PowerIsOn = false, want true")
		}
	})

	t.Run("chassis control", func(t *testing.T) {
		if _, err := cl.ChassisControl(ctx, chassis.ChassisControlPowerUp); err != nil {
			t.Fatalf("power up: %v", err)
		}
		fp.waitCall(t, "on")

		// Soft shutdown has always meant a graceful power-off on this BMC.
		if _, err := cl.ChassisControl(ctx, chassis.ChassisControlSoftShutdown); err != nil {
			t.Fatalf("soft shutdown: %v", err)
		}
		fp.waitCall(t, "off")

		if _, err := cl.ChassisControl(ctx, chassis.ChassisControlPowerCycle); err != nil {
			t.Fatalf("power cycle: %v", err)
		}
		fp.waitCall(t, "reset")
	})

	t.Run("boot options bridge to redfish", func(t *testing.T) {
		set := &types.BootOptionParam_BootFlags{
			BootFlagsValid:     true,
			Persist:            true,
			BootDeviceSelector: bootDevPXE,
		}
		if err := cl.SetSystemBootOptionsParamFor(ctx, set); err != nil {
			t.Fatalf("set boot options: %v", err)
		}
		if ov := redfish.BootOverride(); ov.Target != "Pxe" || ov.Enabled != "Continuous" {
			t.Errorf("redfish override = %+v, want Pxe/Continuous", ov)
		}

		got := &types.BootOptionParam_BootFlags{}
		if err := cl.GetSystemBootOptionsParamFor(ctx, got); err != nil {
			t.Fatalf("get boot options: %v", err)
		}
		if !got.BootFlagsValid || !got.Persist || got.BootDeviceSelector != bootDevPXE {
			t.Errorf("read back = valid=%v persist=%v dev=%#x, want valid PXE persistent",
				got.BootFlagsValid, got.Persist, got.BootDeviceSelector)
		}

		// Clearing from the Redfish side is visible over IPMI: the override
		// state is shared, not mirrored.
		redfish.SetBootOverride("None", "Disabled")
		if err := cl.GetSystemBootOptionsParamFor(ctx, got); err != nil {
			t.Fatalf("get boot options after clear: %v", err)
		}
		if got.BootFlagsValid {
			t.Errorf("BootFlagsValid = true after Redfish clear")
		}
	})

	t.Run("sensors", func(t *testing.T) {
		resp, err := cl.GetSensorReading(ctx, sensorNumSoCTemp)
		if err != nil {
			t.Fatalf("get sensor reading: %v", err)
		}
		if resp.Reading != 55 {
			t.Errorf("SoC temp raw = %d, want 55", resp.Reading)
		}

		resp, err = cl.GetSensorReading(ctx, sensorNumFanDuty)
		if err != nil {
			t.Fatalf("get fan reading: %v", err)
		}
		if resp.Reading != 49 {
			t.Errorf("fan duty raw = %d, want 49", resp.Reading)
		}
	})

	t.Run("sdr repository", func(t *testing.T) {
		sdrs, err := cl.GetSDRs(ctx)
		if err != nil {
			t.Fatalf("get sdrs: %v", err)
		}
		if len(sdrs) != 3 {
			t.Fatalf("got %d SDRs, want 3", len(sdrs))
		}
	})

	t.Run("fru", func(t *testing.T) {
		fru, err := cl.GetFRU(ctx, 0, "")
		if err != nil {
			t.Fatalf("get fru: %v", err)
		}
		if fru == nil {
			t.Fatal("nil FRU")
		}
	})

	t.Run("oem firmware status", func(t *testing.T) {
		resp, err := cl.RawCommand(ctx, oemNetFn, oemCmdGetFirmwareStatus, nil, "oem fw status")
		if err != nil {
			t.Fatalf("oem raw: %v", err)
		}
		if len(resp.Response) != 5 {
			t.Fatalf("oem response length = %d, want 5", len(resp.Response))
		}
		// VolumeReady + Staging set, Presented clear.
		if resp.Response[0] != 0b101 {
			t.Errorf("flags = %#b, want 0b101", resp.Response[0])
		}
		if size := binary.LittleEndian.Uint32(resp.Response[1:]); size != 48<<10 {
			t.Errorf("volume KiB = %d, want %d", size, 48<<10)
		}
	})
}
