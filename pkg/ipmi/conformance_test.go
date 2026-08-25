package ipmi

// conformance_test drives the assembled server through the go-ipmi client the
// way an operator drives ipmitool: power status and control, bootdev, sensor
// list with analog conversion, fru print, raw OEM, user list, and both
// negotiable cipher suites. It replaced a test that shelled out to the real
// ipmitool binary, trading that external dependency for one that always runs;
// the client speaks the same lanplus wire, so the conformance bar is the same.

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"testing"

	"github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

func TestClientConformance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fp := newFakePower(true)
	srv, err := start(ctx, deps{
		port:     0,
		username: "admin",
		password: "admin",
		power:    fp,
		firmware: fakeFirmware{status: firmware.Status{VolumeReady: true, VolumeSize: 48 << 20}},
		broker:   &fakeBroker{},
		sensors:  fakeSensors{reading: sensorReading(t, 61200, 75)},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	redfish.SetBootOverride("None", "Disabled")

	port := srv.Addr().(*net.UDPAddr).Port
	cl, err := client.NewClient("127.0.0.1", port, "admin", "admin")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := cl.Connect(ctx); err != nil {
		t.Fatalf("lanplus connect: %v", err)
	}
	defer cl.Close(ctx)

	t.Run("power status", func(t *testing.T) {
		resp, err := cl.GetChassisStatus(ctx)
		if err != nil {
			t.Fatalf("chassis status: %v", err)
		}
		if !resp.PowerIsOn {
			t.Errorf("PowerIsOn = false, want true")
		}
	})

	t.Run("power off on", func(t *testing.T) {
		if _, err := cl.ChassisControl(ctx, chassis.ChassisControlPowerDown); err != nil {
			t.Fatalf("power off: %v", err)
		}
		fp.waitCall(t, "off")
		if _, err := cl.ChassisControl(ctx, chassis.ChassisControlPowerUp); err != nil {
			t.Fatalf("power on: %v", err)
		}
		fp.waitCall(t, "on")
	})

	t.Run("bootdev", func(t *testing.T) {
		// `ipmitool chassis bootdev pxe`: valid, not persistent.
		set := &types.BootOptionParam_BootFlags{
			BootFlagsValid:     true,
			BootDeviceSelector: bootDevPXE,
		}
		if err := cl.SetSystemBootOptionsParamFor(ctx, set); err != nil {
			t.Fatalf("set bootdev: %v", err)
		}
		if ov := redfish.BootOverride(); ov.Target != "Pxe" || ov.Enabled != "Once" {
			t.Errorf("redfish override = %+v, want Pxe/Once", ov)
		}

		got := &types.BootOptionParam_BootFlags{}
		if err := cl.GetSystemBootOptionsParamFor(ctx, got); err != nil {
			t.Fatalf("get boot options: %v", err)
		}
		if !got.BootFlagsValid || got.Persist || got.BootDeviceSelector != bootDevPXE {
			t.Errorf("read back = valid=%v persist=%v dev=%#x, want valid non-persistent PXE",
				got.BootFlagsValid, got.Persist, got.BootDeviceSelector)
		}
	})

	// The equivalent of `ipmitool sensor list`: walk the SDRs and convert
	// each reading through its record's factors — this is what proves the
	// seeded Full Sensor records carry the right conversion math.
	t.Run("sensor list", func(t *testing.T) {
		sensors, err := cl.GetSensors(ctx)
		if err != nil {
			t.Fatalf("get sensors: %v", err)
		}
		byName := make(map[string]*types.Sensor, len(sensors))
		for _, s := range sensors {
			byName[s.Name] = s
		}

		temp := byName["SoC Temp"]
		if temp == nil {
			t.Fatalf("sensor list has no SoC Temp (got %d sensors)", len(sensors))
		}
		if !temp.HasAnalogReading || !temp.ReadingAvailable {
			t.Errorf("SoC Temp analog=%v available=%v, want both", temp.HasAnalogReading, temp.ReadingAvailable)
		}
		if temp.Value != 61 {
			t.Errorf("SoC Temp = %v, want 61 (61200 milli-C through M=1 factors)", temp.Value)
		}

		fan := byName["Fan Duty"]
		if fan == nil {
			t.Fatalf("sensor list has no Fan Duty")
		}
		if fan.Value != 75 {
			t.Errorf("Fan Duty = %v, want 75", fan.Value)
		}
	})

	t.Run("fru print", func(t *testing.T) {
		fru, err := cl.GetFRU(ctx, 0, "")
		if err != nil {
			t.Fatalf("get fru: %v", err)
		}
		if fru.ProductInfoArea == nil {
			t.Fatal("FRU has no product info area")
		}
		name := types.FRUFieldString(fru.ProductInfoArea.NameTypeLength, fru.ProductInfoArea.Name)
		if name != "NanoKVM BMC" {
			t.Errorf("product name = %q, want %q", name, "NanoKVM BMC")
		}
	})

	t.Run("oem raw", func(t *testing.T) {
		resp, err := cl.RawCommand(ctx, oemNetFn, oemCmdGetFirmwareStatus, nil, "oem fw status")
		if err != nil {
			t.Fatalf("oem raw: %v", err)
		}
		if len(resp.Response) != 5 {
			t.Fatalf("oem response = % x, want 5 bytes", resp.Response)
		}
		if resp.Response[0] != 0x01 {
			t.Errorf("flags = %#x, want 0x01 (volume ready)", resp.Response[0])
		}
		if size := binary.LittleEndian.Uint32(resp.Response[1:]); size != 48<<10 {
			t.Errorf("volume KiB = %d, want %d", size, 48<<10)
		}
	})

	t.Run("user list", func(t *testing.T) {
		users, err := cl.GetUsers(ctx, 1)
		if err != nil {
			t.Fatalf("get users: %v", err)
		}
		found := false
		for _, u := range users {
			if u.Name == "admin" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("user list has no admin (got %d users)", len(users))
		}
	})

	// The old server spoke HMAC-SHA1 only; suite 17 (HMAC-SHA256) is part of
	// what the migration buys, so pin both negotiable suites with their own
	// sessions.
	for _, suite := range []types.CipherSuiteID{types.CipherSuiteID3, types.CipherSuiteID17} {
		t.Run(fmt.Sprintf("cipher suite %d", suite), func(t *testing.T) {
			scl, err := client.NewClient("127.0.0.1", port, "admin", "admin")
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			scl.WithCipherSuiteID(suite)
			if err := scl.Connect(ctx); err != nil {
				t.Fatalf("connect with cipher %v: %v", suite, err)
			}
			defer scl.Close(ctx)
			if _, err := scl.GetChassisStatus(ctx); err != nil {
				t.Errorf("chassis status over cipher %v: %v", suite, err)
			}
		})
	}
}
