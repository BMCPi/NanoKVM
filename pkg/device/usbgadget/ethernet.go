package usbgadget

import (
	"fmt"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// SetEthernet selects the USB ethernet function mode ("off"|"ncm"),
// persists it to the server config, and reconciles the gadget. A mode change
// triggers a UDC unbind/rebind so the host re-enumerates.
func (g *Gadget) SetEthernet(mode string) error {
	switch mode {
	case EthernetOff, EthernetNCM:
	default:
		return fmt.Errorf("invalid ethernet mode %q", mode)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if mode == g.cfg.Ethernet {
		return nil
	}
	if mode != EthernetOff {
		if err := g.ensureEthernetFunc(mode); err != nil {
			return err
		}
	}
	g.cfg.Ethernet = mode
	g.persistHardwareLocked()
	return g.reconcileLinks()
}

// SetDisk toggles whether mass_storage.disk0 is exposed to the host (linked into
// configs/c.1) and persists it to the server config. The function directory and
// its LUNs are never removed.
func (g *Gadget) SetDisk(on bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if on == g.cfg.Disk {
		return nil
	}
	g.cfg.Disk = on
	g.persistHardwareLocked()
	return g.reconcileLinks()
}

// persistHardwareLocked writes the current function toggles back to the server
// config (config is the source of truth) so they survive a restart. Caller
// holds g.mu.
func (g *Gadget) persistHardwareLocked() {
	inst := config.GetInstance()
	inst.UsbGadget.Ethernet = g.cfg.Ethernet
	inst.UsbGadget.Disk = g.cfg.Disk
	config.Save()
}
