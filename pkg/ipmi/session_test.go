package ipmi

import (
	"strings"
	"testing"
)

// TestIPMI_Session_Cipher0 tests RMCP+ session establishment with cipher suite 0
// (no auth, no integrity, no encryption).
func TestIPMI_Session_Cipher0(t *testing.T) {
	_, port := startServer(t)
	out, err := runIPMITool(t, port, 0, "chassis", "status")
	if err != nil {
		t.Fatalf("cipher 0 session failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "System Power") {
		t.Errorf("expected 'System Power' in output, got: %s", out)
	}
}

// TestIPMI_Session_Cipher2 tests RMCP+ session establishment with cipher suite 2
// (HMAC-SHA1 / HMAC-SHA1-96 / no encryption).
func TestIPMI_Session_Cipher2(t *testing.T) {
	_, port := startServer(t)
	out, err := runIPMITool(t, port, 2, "chassis", "status")
	if err != nil {
		t.Fatalf("cipher 2 session failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "System Power") {
		t.Errorf("expected 'System Power' in output, got: %s", out)
	}
}

// TestIPMI_SessionClose tests that sessions can be cleanly closed and reused.
func TestIPMI_SessionClose(t *testing.T) {
	_, port := startServer(t)

	for i := 0; i < 3; i++ {
		out, err := runIPMITool(t, port, 0, "chassis", "status")
		if err != nil {
			t.Fatalf("iteration %d: session failed: %v\noutput: %s", i, err, out)
		}
	}
}

// TestIPMI_Session_IntegrityWrappedCommand tests that commands sent over a cipher 2
// session with integrity wrapping are correctly processed and responded to.
func TestIPMI_Session_IntegrityWrappedCommand(t *testing.T) {
	_, port := startServer(t)
	out, err := runIPMITool(t, port, 2, "chassis", "power", "status")
	if err != nil {
		t.Fatalf("integrity-wrapped command failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Chassis Power is") {
		t.Errorf("expected 'Chassis Power is' in output, got: %s", out)
	}
}
