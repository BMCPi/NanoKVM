package usbgadget

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// gadgetOverTemp points the package's configfs locations at a temp tree and
// returns a Gadget rooted there. Restores the globals via t.Cleanup.
func gadgetOverTemp(t *testing.T) *Gadget {
	t.Helper()
	root := t.TempDir()

	oldSysfs, oldGadget, oldConfigFS := sysfsRootPath, gadgetRoot, configFSPath
	sysfsRootPath = root
	gadgetRoot = filepath.Join(root, "kernel", "config", "usb_gadget")
	configFSPath = filepath.Join(root, "kernel", "config")
	t.Cleanup(func() {
		sysfsRootPath, gadgetRoot, configFSPath = oldSysfs, oldGadget, oldConfigFS
	})

	if err := os.MkdirAll(gadgetRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Gadget{
		fs:  newSysfs(root),
		log: slog.New(slog.DiscardHandler),
		cfg: config.UsbGadget{Ethernet: EthernetNCM},
	}
}

// The live failure this reproduces: on every server restart the gadget is
// already bound, ncm.usb0/host_addr reads back NUL-terminated ("…10:02\x00\n",
// hexdump-verified on the board), the write-if-different guard decides it
// differs, and configfs rejects the rewrite with EBUSY. build() then returns
// early and never reaches reconcileLinks().
//
// The attributes are read-only here, standing in for that EBUSY: if the guard
// misfires and writes, the write fails and the error surfaces.
func TestEnsureEthernetFuncSkipsRewriteOfNULTerminatedMACs(t *testing.T) {
	g := gadgetOverTemp(t)

	dir := filepath.Join(g.functionsPath(), "ncm.usb0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for attr, val := range map[string]string{
		"host_addr": RHIHostMAC,
		"dev_addr":  RHIDevMAC,
	} {
		if err := os.WriteFile(filepath.Join(dir, attr), []byte(val+"\x00\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}

	if err := g.ensureEthernetFunc(EthernetNCM); err != nil {
		t.Fatalf("ensureEthernetFunc rewrote already-matching NUL-terminated MACs: %v", err)
	}
}

// A failure in the ethernet step must not cost the gadget its UDC binding.
//
// This is what turned a cosmetic MAC-comparison bug into a real one: build()
// returned early at the ethernet step, so reconcileLinks() — the only caller of
// ensureBindState() — never ran. The gadget kept working only because it was
// already bound from a previous boot; a gadget left unbound would have stayed
// unbound until the next reboot, with no keyboard and no mass storage.
func TestBuildBindsUDCEvenWhenEthernetStepFails(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{
		Enabled:  true,
		Ethernet: EthernetNCM,
		BindUDC:  true,
		UDCName:  "dummy.udc",
	}

	dir := filepath.Join(g.functionsPath(), "ncm.usb0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Read-only AND holding a different value, so the guard correctly decides a
	// write is needed and the write then fails — standing in for configfs EBUSY.
	if err := os.WriteFile(filepath.Join(dir, "host_addr"), []byte("00:00:00:00:00:00\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	udc := filepath.Join(g.gadgetPath(), "UDC")
	if err := os.MkdirAll(g.gadgetPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(udc, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := g.build(); err == nil {
		t.Fatal("build() reported success despite the ethernet step failing")
	}

	got, err := os.ReadFile(udc)
	if err != nil {
		t.Fatal(err)
	}
	if trimAttr(string(got)) != "dummy.udc" {
		t.Fatalf("UDC = %q, want %q — a failed ethernet step left the gadget unbound",
			got, "dummy.udc")
	}
}
