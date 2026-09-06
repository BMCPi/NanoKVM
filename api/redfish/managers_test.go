package redfish

// managers_test.go covers the Manager's reported build version.

import (
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
)

// The Manager's FirmwareVersion used to come from debug.ReadBuildInfo, whose
// Main.Version is "(devel)" for anything built with `go build` — which is what
// `make app` and `make deploy` do. Every locally deployed node therefore
// reported the "1.0.0" placeholder, and tools/conformance fails a node for
// exactly that. It now reads the version the build actually stamped.
func TestManagerReportsTheStampedVersion(t *testing.T) {
	prev := application.Version
	t.Cleanup(func() { application.Version = prev })

	application.Version = "2.3.16-20-gd21851f"
	got := managerFirmwareVersion()
	if got != "2.3.16-20-gd21851f" {
		t.Errorf("FirmwareVersion = %q, want the stamped build", got)
	}
	if got == "1.0.0" {
		t.Error("reported the unstamped placeholder for a stamped build")
	}
}

// An unstamped build still has to answer something schema-valid rather than an
// empty string, and "dev" is the ldflags default, not a version.
func TestManagerFallsBackWhenUnstamped(t *testing.T) {
	prev := application.Version
	t.Cleanup(func() { application.Version = prev })

	for _, v := range []string{"", "dev"} {
		application.Version = v
		if got := managerFirmwareVersion(); got != "1.0.0" {
			t.Errorf("Version=%q gave FirmwareVersion %q, want the 1.0.0 placeholder", v, got)
		}
	}
}
