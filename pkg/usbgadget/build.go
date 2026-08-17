package usbgadget

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// ensureConfigFS makes sure configfs is mounted at configFSPath. S01fs also
// mounts it at boot; this is a self-sufficiency fallback so the gadget works
// even if the init script did not run. Caller holds g.mu.
func (g *Gadget) ensureConfigFS() error {
	if _, err := g.fs.Stat(gadgetRoot); err == nil {
		return nil // usb_gadget dir present ⇒ configfs mounted and libcomposite loaded
	}
	if isMountPoint(configFSPath) {
		return nil
	}
	if err := g.fs.MkdirAll(configFSPath, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("mount", "-t", "configfs", "configfs", configFSPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount configfs: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// build creates (or reconciles) the full g0 gadget: identity, config, all
// function directories, then the ordered symlink set + UDC bind. Every step is
// idempotent, so a server restart against an already-built gadget is a no-op
// that leaves the bound gadget undisturbed. Caller holds g.mu.
func (g *Gadget) build() error {
	if err := g.ensureGadgetBase(); err != nil {
		return err
	}
	// mass_storage.disk0 (with lun.0 + lun.1) is always created — the firmware
	// controller and virtual-media subsystem require a stable topology — even
	// when the "disk" toggle is off (that only controls the configs/c.1 link).
	if err := g.ensureMassStorageFunc(); err != nil {
		return err
	}
	if g.cfg.HID {
		if err := g.ensureHIDFuncs(); err != nil {
			return err
		}
	}
	if g.cfg.Ethernet != EthernetOff {
		if err := g.ensureEthernetFunc(g.cfg.Ethernet); err != nil {
			return err
		}
	}
	return g.reconcileLinks()
}

// ensureGadgetBase creates the gadget dir, device-descriptor identity, strings
// and config c.1. Attribute writes are skipped when unchanged so re-running on
// a bound gadget does not EBUSY. Caller holds g.mu.
func (g *Gadget) ensureGadgetBase() error {
	gp := g.gadgetPath()
	if err := g.fs.MkdirAll(gp, 0o755); err != nil {
		return fmt.Errorf("create gadget dir: %w", err)
	}
	_ = g.fs.writeAttrIfDifferent(filepath.Join(gp, "idVendor"), g.cfg.VendorID)
	_ = g.fs.writeAttrIfDifferent(filepath.Join(gp, "idProduct"), g.cfg.ProductID)

	strDir := filepath.Join(gp, "strings", "0x409")
	if err := g.fs.MkdirAll(strDir, 0o755); err != nil {
		return fmt.Errorf("create strings dir: %w", err)
	}
	_ = g.fs.writeAttrIfDifferent(filepath.Join(strDir, "serialnumber"), g.cfg.SerialNumber)
	_ = g.fs.writeAttrIfDifferent(filepath.Join(strDir, "manufacturer"), g.cfg.Manufacturer)
	_ = g.fs.writeAttrIfDifferent(filepath.Join(strDir, "product"), g.cfg.Product)

	cp := g.configPath()
	if err := g.fs.MkdirAll(cp, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	_ = g.fs.writeAttrIfDifferent(filepath.Join(cp, "bmAttributes"), g.cfg.BmAttributes)
	_ = g.fs.writeAttrIfDifferent(filepath.Join(cp, "MaxPower"), strconv.Itoa(g.cfg.MaxPower))

	cStrDir := filepath.Join(cp, "strings", "0x409")
	if err := g.fs.MkdirAll(cStrDir, 0o755); err != nil {
		return fmt.Errorf("create config strings dir: %w", err)
	}
	_ = g.fs.writeAttrIfDifferent(filepath.Join(cStrDir, "configuration"), g.cfg.Product)
	return nil
}

// ensureHIDFuncs creates the keyboard/mouse/touchpad function directories and
// writes their attributes + report descriptors. It skips functions that are
// already configured (report_desc non-empty) so a restart does not rewrite a
// live function. Caller holds g.mu.
func (g *Gadget) ensureHIDFuncs() error {
	for _, h := range hidSpecs() {
		dir := filepath.Join(g.functionsPath(), h.name)
		if err := g.fs.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", h.name, err)
		}
		// Already configured by a previous run — leave it alone (writing
		// report_desc on a bound function would EBUSY).
		if cur, err := g.fs.ReadFile(filepath.Join(dir, "report_desc")); err == nil && len(cur) > 0 {
			continue
		}
		if g.cfg.BIOSMode {
			_ = g.fs.writeAttr(filepath.Join(dir, "subclass"), "1")
		}
		if g.cfg.WakeupOnWrite {
			_ = g.fs.writeAttr(filepath.Join(dir, "wakeup_on_write"), "1")
		}
		_ = g.fs.writeAttr(filepath.Join(dir, "protocol"), strconv.Itoa(h.protocol))
		_ = g.fs.writeAttr(filepath.Join(dir, "report_length"), strconv.Itoa(h.reportLength))
		if err := g.fs.writeReportDesc(filepath.Join(dir, "report_desc"), h.reportDesc); err != nil {
			return fmt.Errorf("write %s report_desc: %w", h.name, err)
		}
	}
	return nil
}

// ensureEthernetFunc creates the ncm function directory for mode and pins
// both MAC addresses. The host side MUST be RHIHostMAC: EDK2's UsbNetworkPkg
// driver on the managed host correlates the RHI NIC by station address, and a
// random kernel-assigned MAC breaks that discovery on every boot. Both are
// locally-administered unicast addresses. Caller holds g.mu.
func (g *Gadget) ensureEthernetFunc(mode string) error {
	name := ethernetFuncName(mode)
	if name == "" {
		return nil
	}
	dir := filepath.Join(g.functionsPath(), name)
	if err := g.fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	// Write-if-different: the attributes reject writes (EBUSY) while the
	// gadget is bound, and on a steady-state reconcile they already hold
	// these values from function creation.
	for attr, want := range map[string]string{
		"host_addr": RHIHostMAC,
		"dev_addr":  RHIDevMAC,
	} {
		path := filepath.Join(dir, attr)
		if cur, err := g.fs.ReadFile(path); err == nil &&
			strings.EqualFold(strings.TrimSpace(string(cur)), want) {
			continue
		}
		if err := g.fs.WriteFile(path, []byte(want), 0o644); err != nil {
			return fmt.Errorf("set %s %s: %w", name, attr, err)
		}
	}
	return nil
}

func ethernetFuncName(mode string) string {
	switch mode {
	case EthernetNCM:
		return "ncm.usb0"
	default:
		return ""
	}
}

// reconcileLinks brings the configs/c.1 symlink set in line with the desired
// function set (cfg + state), preserving canonical interface order. It is a
// no-op when the linked set already matches (only the bind state is asserted),
// so a server restart does not disturb the host. Otherwise it unbinds, relinks
// the full desired set in order, sets the OTG role, and rebinds. Caller holds g.mu.
func (g *Gadget) reconcileLinks() error {
	desired := g.desiredFunctions()
	current := g.linkedFunctions()

	if sameSet(desired, current) {
		return g.ensureBindState()
	}

	// Topology change ⇒ full relink. Unbind first so configfs lets us edit the
	// config's function list.
	if err := g.unbindUDCLocked(); err != nil {
		log.Warnf("usbgadget: unbind before relink failed: %v", err)
	}

	// Remove every existing function symlink, then recreate the desired set in
	// canonical order — interface numbering follows symlink creation order.
	for name := range current {
		_ = g.fs.Remove(filepath.Join(g.configPath(), name))
	}
	for _, name := range desired {
		target := filepath.Join(g.functionsPath(), name)
		link := filepath.Join(g.configPath(), name)
		if err := g.fs.Symlink(target, link); err != nil && !os.IsExist(err) {
			return fmt.Errorf("link %s: %w", name, err)
		}
	}

	log.Infof("usbgadget: relinked functions %v", desired)
	return g.ensureBindState()
}

// ensureBindState binds the UDC (per cfg.BindUDC) and asserts the device OTG
// role. Caller holds g.mu.
func (g *Gadget) ensureBindState() error {
	if !g.cfg.BindUDC {
		return nil
	}
	if !g.udcBound() {
		if err := g.bindUDCLocked(); err != nil {
			return err
		}
	}
	if err := g.setOTGRoleLocked("device"); err != nil {
		log.Warnf("usbgadget: set otg role failed: %v", err)
	}
	return nil
}

// desiredFunctions returns the ordered list of functions that should be linked
// into configs/c.1 for the current cfg + state. The order is canonical and MUST
// be preserved: mass_storage → ethernet → keyboard → mouse → touchpad.
func (g *Gadget) desiredFunctions() []string {
	var out []string
	if g.cfg.Disk {
		out = append(out, "mass_storage.disk0")
	}
	if name := ethernetFuncName(g.cfg.Ethernet); name != "" {
		out = append(out, name)
	}
	if g.cfg.HID {
		out = append(out, "hid.GS0", "hid.GS1", "hid.GS2")
	}
	return out
}

// linkedFunctions returns the set of function symlinks currently in configs/c.1
// (excluding the non-function entries: strings, bmAttributes, MaxPower).
func (g *Gadget) linkedFunctions() map[string]bool {
	set := map[string]bool{}
	entries, err := g.fs.ReadDir(g.configPath())
	if err != nil {
		return set
	}
	for _, e := range entries {
		info, err := g.fs.Lstat(filepath.Join(g.configPath(), e.Name()))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		set[e.Name()] = true
	}
	return set
}

// isLinked reports whether name is currently symlinked into configs/c.1.
func (g *Gadget) isLinked(name string) bool {
	info, err := g.fs.Lstat(filepath.Join(g.configPath(), name))
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func sameSet(want []string, have map[string]bool) bool {
	if len(want) != len(have) {
		return false
	}
	for _, name := range want {
		if !have[name] {
			return false
		}
	}
	return true
}
