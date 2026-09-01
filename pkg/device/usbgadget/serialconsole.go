package usbgadget

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// serialFuncName is the configfs function directory for the optional USB
// serial console: f_acm (CDC-ACM), not f_serial (gser).
//
// acm_bind() makes three unguarded usb_ep_autoconfig() calls — its notify
// interrupt-IN is not optional — so it costs 2 IN endpoints where gser costs
// 1. That second endpoint is affordable only because the RHI NIC moved from
// ncm (bulk-IN + notify interrupt-IN, 2) to eem (bulk-IN alone, 1): the swap
// is endpoint-neutral and the composite still lands at exactly 6 (see
// maxINEndpoints). Reverting either half alone overruns the budget.
//
// What the second endpoint buys is a host that binds a driver by itself.
// CDC-ACM is class 0x02/0x02, which cdc_acm matches with a wildcard VID/PID,
// so a Linux host gets /dev/ttyACM0 with no modprobe and no udev rule — the
// thing IPMI SOL actually wants. gser is class 0xFF and matches nothing.
// Both sit on the same u_serial core, so the BMC side is /dev/ttyGS* either
// way.
const serialFuncName = "acm.GS0"

// maxINEndpoints is how many device IN endpoints the SG2002's dwc2 core
// implements. It is a silicon parameter — GHWCFG4's num_dev_in_eps — and no
// device-tree property raises it, so the composite has a hard ceiling of 6 IN
// endpoints regardless of what configfs will let you link.
//
// The board looks like it has seven, and that trap has been walked into once
// already. /sys/kernel/debug/usb/4340000.usb/hw_params reports num_dev_ep 7,
// the driver exposes ep1in..ep7in, and the FIFO layout comes from a
// g-tx-fifo-size array in the DT — all of which suggests declaring a seventh
// entry buys a seventh IN endpoint. It does not. dwc2_hsotg_ep_enable()
// searches `for (i = 1; i <= fifo_count; ++i)` with fifo_count from
// dwc2_hsotg_tx_fifo_count(), which in dedicated-FIFO mode (this core:
// en_multiple_tx_fifo 1) returns num_dev_in_eps and nothing else. DT entries
// past that index are never programmed; the endpoint fails to enable with
// "No suitable fifo found" and -ENOMEM.
//
// To re-measure on a live board without /dev/mem: count the DPTXFIFO lines in
// /sys/kernel/debug/usb/4340000.usb/fifo. fifo_show() bounds that same loop by
// dwc2_hsotg_tx_fifo_count(), so the line count *is* num_dev_in_eps. It reads
// 6.
//
// Overrunning it does not fail at link time. The kernel accepts the symlinks
// and then fails the host's SET_CONFIGURATION with -ENOMEM and a
// "No suitable fifo found" line in dmesg, which reads as a host or cable
// problem. checkEndpointBudget exists to turn that into an error naming the
// functions that did not fit.
const maxINEndpoints = 6

// inEndpointCosts is the per-function IN-endpoint cost, keyed by function
// type (the part before the "." in a configfs instance name).
//
//   - mass_storage: bulk-IN.
//   - ncm/ecm/rndis: bulk-IN plus an interrupt-IN notification endpoint. Any
//     of the three costs the endpoint acm needs, which is why the RHI NIC is
//     eem — see serialFuncName.
//   - eem: bulk-IN only. CDC-EEM has no notification interface at all, so it
//     also has no link-state signalling; the host must assume link-up.
//   - geth: bulk-IN only, like eem, but a vendor-specific class no host binds
//     unclaimed. Not composed today; costed so a future toggle cannot add it
//     silently.
//   - hid: one interrupt-IN per function (this composite has two).
//   - gser: bulk-IN only.
//   - acm: bulk-IN plus an unconditional interrupt-IN.
//
// OUT endpoints are not budgeted: the dwc2 core's OUT direction is not the
// scarce resource, and nothing in this composite comes close to its limit.
var inEndpointCosts = map[string]int{
	"mass_storage": 1,
	"ncm":          2,
	"ecm":          2,
	"rndis":        2,
	"eem":          1,
	"geth":         1,
	"hid":          1,
	"gser":         1,
	"acm":          2,
}

// inEndpointCost returns the IN endpoints one configfs function costs. fn may
// be a full instance name ("eem.usb0") or a bare function type ("eem").
//
// An unrecognised function is charged one endpoint rather than zero: a
// function nobody costed is far more likely to need an endpoint than not, and
// undercounting by one still refuses the sets that are furthest over budget.
func inEndpointCost(fn string) int {
	kind, _, _ := strings.Cut(fn, ".")
	if cost, ok := inEndpointCosts[kind]; ok {
		return cost
	}
	return 1
}

// totalINEndpoints sums the IN-endpoint cost of a function set.
func totalINEndpoints(fns []string) int {
	total := 0
	for _, fn := range fns {
		total += inEndpointCost(fn)
	}
	return total
}

// checkEndpointBudget refuses a function set the UDC cannot serve. The error
// names the total, the ceiling and every function's cost, because the whole
// point is to say what did not fit while the operator can still act on it.
func checkEndpointBudget(fns []string) error {
	total := totalINEndpoints(fns)
	if total <= maxINEndpoints {
		return nil
	}

	costs := make([]string, 0, len(fns))
	for _, fn := range fns {
		costs = append(costs, fmt.Sprintf("%s=%d", fn, inEndpointCost(fn)))
	}
	return fmt.Errorf(
		"usb IN-endpoint budget exceeded: %d needed, %d available (SG2002 dwc2 GHWCFG4 num_dev_in_eps, a silicon limit); functions: %s",
		total, maxINEndpoints, strings.Join(costs, ", "))
}

// ensureSerialFunc creates the acm function directory. f_acm has no writable
// attributes — u_serial allocates its port at function-instance creation and
// reports it read-only in port_num — so this is a bare, idempotent mkdir.
// Caller holds g.mu.
func (g *Gadget) ensureSerialFunc() error {
	dir := filepath.Join(g.functionsPath(), serialFuncName)
	if err := g.fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", serialFuncName, err)
	}
	return nil
}

// SerialConsoleDevice returns the BMC-side device node of the USB serial
// console — /dev/ttyGS<port_num> — or "" when the console is not actually
// composed. pkg/device/serial resolves the port it opens through this at open
// time; nothing persists the path.
//
// The port number is read back from configfs rather than assumed to be 0:
// u_serial numbers its ports by allocation order across every f_serial/f_acm
// instance, so acm.GS0 is only ttyGS0 while it is the first one allocated.
func (g *Gadget) SerialConsoleDevice() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Checked before touching the tree: the function directory outlives a
	// toggle-off (reconcileLinks only drops the symlink), so a stale port_num
	// must not resurrect a console the operator turned off.
	if !g.cfg.SerialConsole {
		return ""
	}

	// The toggle says what was asked for; the symlink says what the gadget is
	// actually presenting. Answering from the toggle alone hands the broker a
	// device node for a function that was never linked — because creating it
	// failed, or because reconcileLinks refused the set on the endpoint budget
	// — and the broker then abandons the operator's real serial.device for a
	// console that does not exist. isLinked reads the same configfs state
	// linkedFunctions() does.
	if !g.isLinked(serialFuncName) {
		return ""
	}

	raw, err := g.fs.ReadFile(filepath.Join(g.functionsPath(), serialFuncName, "port_num"))
	if err != nil {
		return ""
	}
	port, err := strconv.Atoi(trimAttr(string(raw)))
	if err != nil || port < 0 {
		return ""
	}
	return fmt.Sprintf("/dev/ttyGS%d", port)
}

// SetSerialConsole composes (or drops) the USB serial function and persists
// the choice. Like the other function toggles this relinks the config, so the
// host re-enumerates the device. The caller is responsible for restarting the
// serial broker afterwards — the console device changes with the topology.
//
// Turning it off leaves functions/acm.GS0 in place and only removes the
// symlink, the same way mass_storage.disk0 is kept: removing a u_serial
// function instance releases its port number, which would renumber every
// other ttyGS on the system.
func (g *Gadget) SetSerialConsole(on bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if on == g.cfg.SerialConsole {
		return nil
	}
	if on {
		if err := g.ensureSerialFunc(); err != nil {
			return err
		}
	}

	// Reconcile first, persist only on success — unlike SetEthernet and
	// SetDisk, which persist first. This toggle also decides which port the
	// broker opens, so persisting a reconcile that failed leaves the settings
	// panel showing "on" and SerialConsoleDevice() resolving a console the
	// gadget never composed, while the terminal and SOL are still on
	// serial.device. desiredFunctions() reads g.cfg, so the flag has to be set
	// before the reconcile; it is rolled back if that fails.
	previous := g.cfg.SerialConsole
	g.cfg.SerialConsole = on
	if err := g.reconcileLinks(); err != nil {
		g.cfg.SerialConsole = previous
		return err
	}
	g.persistHardwareLocked()
	return nil
}
