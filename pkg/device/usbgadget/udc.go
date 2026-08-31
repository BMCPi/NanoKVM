package usbgadget

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

const udcClassPath = "/sys/class/udc"

// dwc2 platform driver paths for PHY recovery (S03usbdev restart_phy parity).
const (
	dwc2UnbindPath = "/sys/bus/platform/drivers/dwc2/unbind"
	dwc2BindPath   = "/sys/bus/platform/drivers/dwc2/bind"
)

// udcName returns the UDC to bind: cfg.UDCName if set, else the first entry in
// /sys/class/udc (this board's dwc2 controller is "4340000.usb").
func (g *Gadget) udcName() (string, error) {
	if g.cfg.UDCName != "" {
		return g.cfg.UDCName, nil
	}
	entries, err := g.fs.ReadDir(udcClassPath)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", udcClassPath, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no UDC found in %s", udcClassPath)
	}
	sort.Strings(names)
	return names[0], nil
}

// UDCBound reports whether a UDC is currently bound to the gadget.
func (g *Gadget) UDCBound() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.udcBoundLocked()
}

func (g *Gadget) udcBoundLocked() bool {
	data, err := g.fs.ReadFile(g.udcPath())
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}

// bindUDCLocked writes the UDC name into g0/UDC. Caller holds g.mu.
func (g *Gadget) bindUDCLocked() error {
	udc, err := g.udcName()
	if err != nil {
		return err
	}
	if err := g.fs.writeAttr(g.udcPath(), udc); err != nil {
		return fmt.Errorf("bind UDC %s: %w", udc, err)
	}
	return nil
}

// unbindUDCLocked clears g0/UDC and waits until the kernel confirms release.
// Writing "" is asynchronous on this kernel, so poll (20 × 50 ms) until the UDC
// file reads back empty before any topology mutation. Caller holds g.mu.
func (g *Gadget) unbindUDCLocked() error {
	if !g.udcBoundLocked() {
		return nil
	}
	if err := g.fs.writeAttr(g.udcPath(), "\n"); err != nil {
		return fmt.Errorf("clear UDC: %w", err)
	}
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		if !g.udcBoundLocked() {
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for UDC to unbind")
}

// RebindUDC unbinds and rebinds the UDC to force the host to re-enumerate.
// Replaces the old firmware.resetUDC.
func (g *Gadget) RebindUDC() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rebindUDCLocked()
}

func (g *Gadget) rebindUDCLocked() error {
	if err := g.unbindUDCLocked(); err != nil {
		g.log.Warn("usbgadget: unbind during rebind", slog.Any("err", err))
	}
	time.Sleep(200 * time.Millisecond)
	return g.bindUDCLocked()
}

// SetOTGRole sets the CVITEK/Sophgo OTG role ("device"|"host").
func (g *Gadget) SetOTGRole(role string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.setOTGRoleLocked(role)
}

func (g *Gadget) setOTGRoleLocked(role string) error {
	if g.cfg.OTGRolePath == "" {
		return nil
	}
	// OTGRolePath is the CVITEK OTG switch under /proc (default
	// /proc/cviusb/otg_role), not /sys, so it is written with plain os rather
	// than the sysfs-root service.
	if err := os.WriteFile(g.cfg.OTGRolePath, []byte(role), 0o644); err != nil { //nolint:gosec // kernel pseudo-file under /proc; the mode arg only applies at O_CREATE and the node already exists with a mode the kernel owns, so narrowing it here is inert
		return fmt.Errorf("set otg role %s: %w", role, err)
	}
	return nil
}

// RebindPHY rebinds the dwc2 platform driver to recover a wedged controller.
// Mirrors the old S03usbdev restart_phy action.
func (g *Gadget) RebindPHY() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	dev := g.cfg.PHYDevice
	if dev == "" {
		return fmt.Errorf("phy device not configured")
	}
	if err := g.fs.writeAttr(dwc2UnbindPath, dev); err != nil {
		return fmt.Errorf("dwc2 unbind %s: %w", dev, err)
	}
	time.Sleep(1 * time.Second)
	if err := g.fs.writeAttr(dwc2BindPath, dev); err != nil {
		return fmt.Errorf("dwc2 bind %s: %w", dev, err)
	}
	g.log.Info("usbgadget: rebound dwc2 PHY", slog.String("device", dev))
	return nil
}
