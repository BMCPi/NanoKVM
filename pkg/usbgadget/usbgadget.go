// Package usbgadget is the single owner of the USB device gadget's configfs
// tree (/sys/kernel/config/usb_gadget/g0). It builds the gadget at server
// startup and mutates it at runtime — replacing the old split between the
// packaging/etc/init.d/S03usbdev shell script (which built the gadget at boot)
// and ad-hoc Go/shell runtime edits scattered across the firmware and vm
// services. The Go server ("kvmapp") is now the only thing that touches the
// gadget configfs.
//
// The design mirrors JetKVM's usbgadget package in shape — a declarative set of
// functions, an ordered idempotent build/reconcile, and UDC/OTG control — but
// is tailored to this board's SG2002/CVITEK hardware (dwc2 UDC "4340000.usb",
// /proc/cviusb/otg_role, dwc2 PHY rebind, and the fsg_bind LUN ordering
// constraint) and carries no extra dependencies.
package usbgadget

import (
	"fmt"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// configfs locations. These are package vars (not consts) so tests can point
// them at a temporary tree. sysfsRootPath is the os.Root every /sys operation is
// confined to (see the sysfs service); a test that exercises the configfs tree
// overrides it to a temp prefix that contains the overridden gadgetRoot.
var (
	configFSPath  = "/sys/kernel/config"
	gadgetRoot    = "/sys/kernel/config/usb_gadget"
	sysfsRootPath = "/sys"
)

const gadgetName = "g0"

// Ethernet function modes.
const (
	EthernetOff = "off"
	EthernetNCM = "ncm"

	// The RHI link's MAC pair. The host side is fixed by contract with the
	// managed host's EDK2 firmware (UsbNetworkPkg), which identifies the
	// Redfish host interface by this station address; the device side is
	// its deterministic counterpart so neighbor caches survive reboots.
	RHIHostMAC = "da:c0:ff:ee:10:02"
	RHIDevMAC  = "da:c0:ff:ee:10:01"
)

// Gadget owns the g0 configfs tree. A single mutex serializes every configfs
// mutation, the same discipline firmware.Controller uses for its own state. All
// of its /sys reads and writes go through fs, an os.Root-scoped sysfs service,
// so no configfs symlink can steer an operation outside /sys.
type Gadget struct {
	mu  sync.Mutex
	cfg config.UsbGadget
	fs  *sysfs
}

var (
	instance *Gadget
	once     sync.Once
)

// Get returns the singleton Gadget.
func Get() *Gadget {
	once.Do(func() {
		instance = &Gadget{
			cfg: config.GetInstance().UsbGadget,
			fs:  newSysfs(sysfsRootPath),
		}
	})
	return instance
}

// Init migrates the legacy /boot flags on first run, then builds g0 and binds
// it. It is idempotent: when the gadget already exists and is correct (the
// common server-restart case) it leaves the bound gadget undisturbed rather
// than re-enumerating the host. No-op when disabled in config. Call once at
// server startup, before firmware.Controller.Init presents the boot image.
func (g *Gadget) Init() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// The config is the source of truth for the whole gadget topology. (The
	// old /data/usbgadget/state.json fold-in is gone: /data no longer exists
	// on the squashfs+overlay layout, so that migration could never fire.)
	g.cfg = config.GetInstance().UsbGadget

	if !g.cfg.Enabled {
		log.Info("usbgadget: disabled by config; leaving gadget untouched")
		return nil
	}

	if err := g.ensureConfigFS(); err != nil {
		return fmt.Errorf("ensure configfs: %w", err)
	}
	if err := g.build(); err != nil {
		return fmt.Errorf("build gadget: %w", err)
	}

	log.Infof("usbgadget: g0 ready (vid=%s pid=%s hid=%v ethernet=%s disk=%v udc-bound=%v)",
		g.cfg.VendorID, g.cfg.ProductID, g.cfg.HID, g.cfg.Ethernet, g.cfg.Disk, g.udcBound())
	return nil
}

// State returns a thread-safe snapshot of the function toggles from config.
func (g *Gadget) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return State{Ethernet: g.cfg.Ethernet, Disk: g.cfg.Disk}
}

// ---- configfs path helpers -------------------------------------------------

func (g *Gadget) gadgetPath() string    { return filepath.Join(gadgetRoot, gadgetName) }
func (g *Gadget) configPath() string    { return filepath.Join(g.gadgetPath(), "configs", "c.1") }
func (g *Gadget) functionsPath() string { return filepath.Join(g.gadgetPath(), "functions") }
func (g *Gadget) udcPath() string       { return filepath.Join(g.gadgetPath(), "UDC") }
