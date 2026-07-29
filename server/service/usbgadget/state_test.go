package usbgadget

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pi-bmc/nanokvm-app/server/config"
)

// base returns a config as it would arrive with identity + toggle defaults.
func base() config.UsbGadget {
	return config.UsbGadget{VendorID: "0x3346", ProductID: "0x1009", Ethernet: EthernetECM, Disk: true}
}

func TestApplyLegacyStateFoldsEthernetAndDisk(t *testing.T) {
	got := applyLegacyState(base(), State{Ethernet: EthernetNCM, Disk: false})
	if got.Ethernet != EthernetNCM {
		t.Errorf("ethernet = %q, want %q", got.Ethernet, EthernetNCM)
	}
	if got.Disk {
		t.Error("disk = true, want false (legacy state disabled it)")
	}
}

func TestApplyLegacyStateIgnoresUnknownEthernet(t *testing.T) {
	// A garbage ethernet value must not clobber the config's mode; disk still
	// copies over.
	got := applyLegacyState(base(), State{Ethernet: "bogus", Disk: false})
	if got.Ethernet != EthernetECM {
		t.Errorf("ethernet = %q, want the config value %q kept", got.Ethernet, EthernetECM)
	}
	if got.Disk {
		t.Error("disk = true, want false")
	}
}

func TestLoadLegacyStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"ethernet":"ncm","disk":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := loadLegacyState(path)
	if !ok {
		t.Fatal("loadLegacyState: ok=false for a valid file")
	}
	if got.Ethernet != EthernetNCM || got.Disk {
		t.Fatalf("loadLegacyState = %+v, want {ncm false}", got)
	}
}

func TestLoadLegacyStateAbsent(t *testing.T) {
	if _, ok := loadLegacyState(filepath.Join(t.TempDir(), "missing.json")); ok {
		t.Fatal("loadLegacyState: ok=true for an absent file")
	}
}

func TestLoadLegacyStateCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadLegacyState(path); ok {
		t.Fatal("loadLegacyState: ok=true on corrupt JSON")
	}
}
