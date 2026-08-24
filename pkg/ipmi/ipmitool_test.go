package ipmi

// ipmitool_test drives the server with the real ipmitool binary — the
// conformance bar that matters operationally. Skipped where ipmitool is not
// installed.

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

func TestIpmitool(t *testing.T) {
	if _, err := exec.LookPath("ipmitool"); err != nil {
		t.Skip("ipmitool not installed")
	}

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
	run := func(t *testing.T, args ...string) string {
		t.Helper()
		base := []string{"-I", "lanplus", "-H", "127.0.0.1", "-p", fmt.Sprint(port),
			"-U", "admin", "-P", "admin"}
		cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
		defer ccancel()
		out, err := exec.CommandContext(cctx, "ipmitool", append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("ipmitool %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	t.Run("power status", func(t *testing.T) {
		if out := run(t, "power", "status"); !strings.Contains(out, "Chassis Power is on") {
			t.Errorf("power status = %q", out)
		}
	})

	t.Run("power off on", func(t *testing.T) {
		run(t, "power", "off")
		fp.waitCall(t, "off")
		run(t, "power", "on")
		fp.waitCall(t, "on")
	})

	t.Run("bootdev", func(t *testing.T) {
		run(t, "chassis", "bootdev", "pxe")
		if ov := redfish.BootOverride(); ov.Target != "Pxe" {
			t.Errorf("redfish override target = %q, want Pxe", ov.Target)
		}
		if out := run(t, "chassis", "bootparam", "get", "5"); !strings.Contains(out, "PXE") {
			t.Errorf("bootparam get 5 = %q, want PXE", out)
		}
	})

	t.Run("sensor list", func(t *testing.T) {
		out := run(t, "sensor", "list")
		if !strings.Contains(out, "SoC Temp") || !strings.Contains(out, "61") {
			t.Errorf("sensor list missing SoC Temp @ 61C:\n%s", out)
		}
		if !strings.Contains(out, "Fan Duty") || !strings.Contains(out, "75") {
			t.Errorf("sensor list missing Fan Duty @ 75%%:\n%s", out)
		}
	})

	t.Run("fru print", func(t *testing.T) {
		out := run(t, "fru", "print", "0")
		if !strings.Contains(out, "NanoKVM BMC") {
			t.Errorf("fru print = %q, want product name", out)
		}
	})

	t.Run("oem raw", func(t *testing.T) {
		out := strings.TrimSpace(run(t, "raw", "0x30", "0x01"))
		// flags=0x01 (volume ready), size KiB = 48<<10 = 0xC000 LE.
		if !strings.HasPrefix(out, "01 00 c0 00 00") {
			t.Errorf("oem raw = %q, want '01 00 c0 00 00'", out)
		}
	})

	t.Run("user list", func(t *testing.T) {
		if out := run(t, "user", "list", "1"); !strings.Contains(out, "admin") {
			t.Errorf("user list = %q, want admin present", out)
		}
	})

	// The old server spoke HMAC-SHA1 only; suite 17 (HMAC-SHA256) is part of
	// what the migration buys, so pin both negotiable suites.
	for _, suite := range []string{"3", "17"} {
		t.Run("cipher suite "+suite, func(t *testing.T) {
			if out := run(t, "-C", suite, "power", "status"); !strings.Contains(out, "Chassis Power") {
				t.Errorf("cipher %s power status = %q", suite, out)
			}
		})
	}
}
