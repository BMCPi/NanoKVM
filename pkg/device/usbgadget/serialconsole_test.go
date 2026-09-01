package usbgadget

// serialconsole_test.go covers the optional CDC-ACM USB serial function
// (acm.GS0): the IN-endpoint budget that decides what can be composed at all,
// where acm lands in the canonical function order, and how the console device
// node is resolved.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// The composite without the console: 4 of the 6 IN endpoints the SG2002's
// dwc2 core implements. Adding acm lands exactly on the limit.
func TestEndpointBudgetCurrentCompositeFits(t *testing.T) {
	current := []string{"mass_storage.disk0", "eem.usb0", "hid.GS0", "hid.GS1"}

	if got := totalINEndpoints(current); got != 4 {
		t.Fatalf("current composite costs %d IN endpoints, want 4", got)
	}
	if err := checkEndpointBudget(current); err != nil {
		t.Fatalf("current composite refused: %v", err)
	}
}

func TestEndpointBudgetWithACMIsExactlyAtLimit(t *testing.T) {
	withACM := []string{"mass_storage.disk0", "eem.usb0", "hid.GS0", "hid.GS1", "acm.GS0"}

	if got := totalINEndpoints(withACM); got != maxINEndpoints {
		t.Fatalf("composite with acm costs %d IN endpoints, want %d", got, maxINEndpoints)
	}
	if err := checkEndpointBudget(withACM); err != nil {
		t.Fatalf("composite with acm refused, but it fits at %d/%d: %v",
			maxINEndpoints, maxINEndpoints, err)
	}
}

// acm fits only because the RHI NIC gave back an endpoint. Pair it with a
// notify-carrying network function — ncm, ecm or rndis — and the composite is
// 7, one past what the silicon serves. The refusal must say so in words, not
// leave the kernel to fail the SET_CONFIGURATION with "No suitable fifo found".
func TestEndpointBudgetRefusesACMAlongsideNCM(t *testing.T) {
	withACM := []string{"mass_storage.disk0", "ncm.usb0", "hid.GS0", "hid.GS1", "acm.GS0"}

	if got := totalINEndpoints(withACM); got != 7 {
		t.Fatalf("composite with ncm+acm costs %d IN endpoints, want 7", got)
	}
	err := checkEndpointBudget(withACM)
	if err == nil {
		t.Fatal("checkEndpointBudget accepted a 7-endpoint composite")
	}
	msg := err.Error()
	for _, want := range []string{"7", "6", "acm.GS0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q (it must name the total and the offenders)", msg, want)
		}
	}
}

// Two network functions is the other realistic way to overrun the budget
// (ncm + rndis = 4 IN endpoints on their own).
func TestEndpointBudgetRefusesTwoNetworkFunctions(t *testing.T) {
	twoNets := []string{"mass_storage.disk0", "ncm.usb0", "rndis.usb1", "hid.GS0", "hid.GS1"}

	err := checkEndpointBudget(twoNets)
	if err == nil {
		t.Fatal("checkEndpointBudget accepted ncm + rndis alongside the rest of the composite")
	}
	if !strings.Contains(err.Error(), "rndis.usb1") || !strings.Contains(err.Error(), "ncm.usb0") {
		t.Errorf("error %q does not name both network functions", err)
	}
}

func TestINEndpointCost(t *testing.T) {
	for _, tc := range []struct {
		fn   string
		want int
	}{
		{"mass_storage.disk0", 1},
		{"ncm.usb0", 2},
		{"ecm.usb0", 2},
		{"rndis.usb0", 2},
		{"eem.usb0", 1},
		{"geth.usb0", 1},
		{"hid.GS0", 1},
		{"gser.GS0", 1},
		{"acm.GS0", 2},
		// Bare instance-less names are accepted too — the table is keyed by
		// function type, and the budget is computed before any instance exists.
		{"gser", 1},
		// An unknown function is charged the one-endpoint floor rather than
		// being waved through at zero.
		{"future.0", 1},
	} {
		if got := inEndpointCost(tc.fn); got != tc.want {
			t.Errorf("inEndpointCost(%q) = %d, want %d", tc.fn, got, tc.want)
		}
	}
}

// The whole point of the budget guard is that it tracks what the gadget can
// actually ask for: the maximal set every toggle can produce must still fit.
func TestMaximalConfigFitsTheBudget(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{
		Enabled:       true,
		Disk:          true,
		Ethernet:      EthernetEEM,
		HID:           true,
		SerialConsole: true,
	}

	desired := g.desiredFunctions()
	if err := checkEndpointBudget(desired); err != nil {
		t.Fatalf("the maximal configuration is over budget: %v (%v)", err, desired)
	}
}

// acm goes LAST. Interface numbering follows symlink creation order, and
// reconcileLinks does a full unbind/relink — i.e. a host re-enumeration — on
// any topology change, so the canonical order may be extended, never reordered.
func TestDesiredFunctionsAppendsACMLast(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{
		Disk:          true,
		Ethernet:      EthernetEEM,
		HID:           true,
		SerialConsole: true,
	}

	want := []string{"mass_storage.disk0", "eem.usb0", "hid.GS0", "hid.GS1", "acm.GS0"}
	if got := g.desiredFunctions(); !slices.Equal(got, want) {
		t.Fatalf("desiredFunctions() = %v, want %v", got, want)
	}
}

func TestDesiredFunctionsOmitsACMWhenDisabled(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Disk: true, Ethernet: EthernetEEM, HID: true}

	want := []string{"mass_storage.disk0", "eem.usb0", "hid.GS0", "hid.GS1"}
	if got := g.desiredFunctions(); !slices.Equal(got, want) {
		t.Fatalf("desiredFunctions() = %v, want %v", got, want)
	}
}

// build() must create the function directory the symlink will point at.
func TestBuildCreatesGserFunction(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true, SerialConsole: true}

	if err := g.build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	dir := filepath.Join(g.functionsPath(), serialFuncName)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("functions/%s not created: %v", serialFuncName, err)
	}
	if !g.isLinked(serialFuncName) {
		t.Fatalf("%s was created but never linked into configs/c.1", serialFuncName)
	}
}

func TestBuildSkipsGserWhenDisabled(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true}

	if err := g.build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.functionsPath(), serialFuncName)); err == nil {
		t.Fatalf("functions/%s created with serialConsole off", serialFuncName)
	}
}

// The budget guard has to sit in the path that actually links functions, not
// only in a helper the tests call directly: deleting the check from
// reconcileLinks must break something. This drives an over-budget set through
// reconcileLinks and asserts both that it is refused and that the tree was left
// alone — the set that did fit stays linked rather than being torn down for one
// the UDC cannot serve.
func TestReconcileLinksRefusesAnOverBudgetSet(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true, Disk: true, Ethernet: EthernetEEM, HID: true}

	if err := g.build(); err != nil {
		t.Fatalf("build the in-budget composite: %v", err)
	}
	linked := g.linkedFunctions()
	if len(linked) != 4 {
		t.Fatalf("in-budget composite linked %v, want 4 functions", linked)
	}

	// A second network function is the realistic way to overrun the budget;
	// desiredFunctions cannot produce one, so inject it the way a new toggle
	// would and drive the real link path.
	orig := inEndpointCosts["hid"]
	inEndpointCosts["hid"] = 3 // as if someone re-added the separate touchpad
	t.Cleanup(func() { inEndpointCosts["hid"] = orig })

	err := g.reconcileLinks()
	if err == nil {
		t.Fatal("reconcileLinks accepted a set over the IN-endpoint budget")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("reconcileLinks error %q does not name the budget", err)
	}

	// Refused before the tree was touched: whatever was linked is still linked.
	if after := g.linkedFunctions(); len(after) != len(linked) {
		t.Fatalf("a refused reconcile changed the linked set: %v → %v", linked, after)
	}
}

// The device node is read back from port_num rather than hardcoded: u_serial
// numbers its ports by allocation order, so acm.GS0 is not necessarily
// ttyGS0 once anything else claims a u_serial port first.
func TestSerialConsoleDeviceReadsPortNum(t *testing.T) {
	for _, tc := range []struct {
		name    string
		portNum string
		want    string
	}{
		{"port 0", "0\n", "/dev/ttyGS0"},
		{"port 1", "1\n", "/dev/ttyGS1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gadgetOverTemp(t)
			g.cfg = config.UsbGadget{Enabled: true, SerialConsole: true}
			writeACMPortNum(t, g, tc.portNum)
			linkACM(t, g)

			if got := g.SerialConsoleDevice(); got != tc.want {
				t.Fatalf("SerialConsoleDevice() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSerialConsoleDeviceEmptyWhenDisabled(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true}
	// The function directory survives a toggle-off (only the symlink goes),
	// so a stale port_num must not resurrect the console device.
	writeACMPortNum(t, g, "0\n")

	if got := g.SerialConsoleDevice(); got != "" {
		t.Fatalf("SerialConsoleDevice() = %q with serialConsole off, want \"\"", got)
	}
}

func TestSerialConsoleDeviceEmptyWhenFunctionMissing(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true, SerialConsole: true}

	if got := g.SerialConsoleDevice(); got != "" {
		t.Fatalf("SerialConsoleDevice() = %q with no acm function, want \"\"", got)
	}
}

// The toggle is what the operator asked for; the symlink is what the gadget is
// presenting. When the function exists and reports a port but was never linked
// — the link failed, or reconcileLinks refused the set on the endpoint budget —
// the broker must keep the operator's serial.device rather than be handed a
// dead node for a console the host cannot see.
func TestSerialConsoleDeviceEmptyWhenFunctionIsNotLinked(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true, SerialConsole: true}
	writeACMPortNum(t, g, "0\n")

	if got := g.SerialConsoleDevice(); got != "" {
		t.Fatalf("SerialConsoleDevice() = %q for an unlinked acm function, want \"\"", got)
	}

	// And once it really is linked, the same tree answers.
	linkACM(t, g)
	if got := g.SerialConsoleDevice(); got != "/dev/ttyGS0" {
		t.Fatalf("SerialConsoleDevice() = %q once acm is linked, want /dev/ttyGS0", got)
	}
}

// A port_num the kernel never wrote (or that something truncated) must not
// produce "/dev/ttyGS" and send the broker off to open a nonexistent node.
func TestSerialConsoleDeviceEmptyOnUnparseablePortNum(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true, SerialConsole: true}
	writeACMPortNum(t, g, "\n")
	linkACM(t, g)

	if got := g.SerialConsoleDevice(); got != "" {
		t.Fatalf("SerialConsoleDevice() = %q for an empty port_num, want \"\"", got)
	}
}

// A failed reconcile must not leave the toggle persisted as on: the panel would
// claim a console the gadget never composed while the broker stayed on
// serial.device.
func TestSetSerialConsoleRollsBackWhenReconcileFails(t *testing.T) {
	g := gadgetOverTemp(t)
	g.cfg = config.UsbGadget{Enabled: true, Disk: true, Ethernet: EthernetEEM, HID: true}
	if err := g.build(); err != nil {
		t.Fatalf("build: %v", err)
	}

	// Make the acm link push the composite over budget, so reconcileLinks
	// refuses it — the same refusal an over-budget set gets in production.
	// acm already costs 2 of the 6 and the rest of the composite costs 4, so
	// it takes a third endpoint to overrun.
	orig := inEndpointCosts["acm"]
	inEndpointCosts["acm"] = 3
	t.Cleanup(func() { inEndpointCosts["acm"] = orig })

	if err := g.SetSerialConsole(true); err == nil {
		t.Fatal("SetSerialConsole succeeded on a set reconcileLinks must refuse")
	}
	if g.cfg.SerialConsole {
		t.Error("SetSerialConsole left the toggle on after the reconcile failed")
	}
	if got := g.SerialConsoleDevice(); got != "" {
		t.Errorf("SerialConsoleDevice() = %q after a failed enable, want \"\"", got)
	}
}

func writeACMPortNum(t *testing.T, g *Gadget, value string) {
	t.Helper()

	dir := filepath.Join(g.functionsPath(), serialFuncName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "port_num"), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linkACM symlinks acm.GS0 into configs/c.1, which is what reconcileLinks
// does when the function is actually composed.
func linkACM(t *testing.T, g *Gadget) {
	t.Helper()

	if err := os.MkdirAll(g.configPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	err := os.Symlink(filepath.Join(g.functionsPath(), serialFuncName),
		filepath.Join(g.configPath(), serialFuncName))
	if err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
}
