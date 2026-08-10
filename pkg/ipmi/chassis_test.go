package ipmi

import (
	"strings"
	"testing"
)

// TestIPMI_ChassisStatus tests handleGetChassisStatus via ipmitool.
func TestIPMI_ChassisStatus(t *testing.T) {
	_, port := startServer(t)
	out, err := runIPMITool(t, port, 0, "chassis", "status")
	if err != nil {
		t.Fatalf("ipmitool chassis status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "System Power") {
		t.Errorf("expected 'System Power' in output, got: %s", out)
	}
}

// TestIPMI_ChassisControl tests handleChassisControl for all power actions.
func TestIPMI_ChassisControl(t *testing.T) {
	_, port := startServer(t)

	tests := []struct {
		name string
		args []string
	}{
		{"power_on", []string{"chassis", "power", "on"}},
		{"power_off", []string{"chassis", "power", "off"}},
		{"power_cycle", []string{"chassis", "power", "cycle"}},
		{"power_reset", []string{"chassis", "power", "reset"}},
		{"soft_shutdown", []string{"chassis", "power", "soft"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runIPMITool(t, port, 0, tc.args...)
			if err != nil {
				t.Fatalf("ipmitool %v failed: %v\noutput: %s", tc.args, err, out)
			}
		})
	}
}

// TestIPMI_BootDevice tests handleSetSystemBootOptions and handleGetSystemBootOptions.
func TestIPMI_BootDevice(t *testing.T) {
	_, port := startServer(t)

	// Set boot device to PXE
	out, err := runIPMITool(t, port, 0, "chassis", "bootdev", "pxe")
	if err != nil {
		t.Fatalf("set bootdev pxe failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Set Boot Device to pxe") {
		t.Errorf("expected set boot device confirmation, got: %s", out)
	}

	// Set boot device to disk
	out, err = runIPMITool(t, port, 0, "chassis", "bootdev", "disk")
	if err != nil {
		t.Fatalf("set bootdev disk failed: %v\noutput: %s", err, out)
	}
}

// TestIPMI_MCInfo tests handleGetDeviceID via ipmitool mc info.
func TestIPMI_MCInfo(t *testing.T) {
	_, port := startServer(t)
	out, err := runIPMITool(t, port, 0, "mc", "info")
	if err != nil {
		t.Fatalf("ipmitool mc info failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Device ID") {
		t.Errorf("expected 'Device ID' in output, got: %s", out)
	}
	if !strings.Contains(out, "IPMI Version") {
		t.Errorf("expected 'IPMI Version' in output, got: %s", out)
	}
}
