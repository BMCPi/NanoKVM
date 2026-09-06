package components

// settings_hardware_test.go guards which form the USB serial console switch
// lives in.
//
// The Hardware panel has three forms with different endpoints, and the
// difference is invisible in the rendered page: the "Functions" form posts to
// /ui/settings/hardware, which reconciles the gadget AND restarts the serial
// broker, while the two below it only persist settings for the next boot. A
// serial-console switch that drifted into one of those would look like it
// worked — the switch would move and the value would stick — while the gadget
// kept its old composite and the console stayed on the old device.

import (
	"context"
	"strings"
	"testing"
)

func renderHardwarePanel(t *testing.T, m SettingsHardware) string {
	t.Helper()

	var sb strings.Builder
	if err := SettingsHardwareBody(m).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// functionsForm returns the markup of the form that posts to
// /ui/settings/hardware (the live-applied one), or fails the test.
func functionsForm(t *testing.T, html string) string {
	t.Helper()

	const marker = `hx-post="/ui/settings/hardware"`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("no form posting to /ui/settings/hardware in the panel")
	}
	rest := html[i:]
	end := strings.Index(rest, "</form>")
	if end < 0 {
		t.Fatal("the functions form is never closed")
	}
	return rest[:end]
}

func TestSerialConsoleSwitchIsInTheLiveAppliedForm(t *testing.T) {
	html := renderHardwarePanel(t, SettingsHardware{ConsoleDevice: "/dev/ttyGS0"})

	form := functionsForm(t, html)
	if !strings.Contains(form, `name="serialConsole"`) {
		t.Errorf("the serialConsole switch is not in the form that reconciles the gadget:\n%s", form)
	}
	// Its neighbours must still be there — this form owns every live-applied
	// function toggle.
	for _, name := range []string{`name="network"`, `name="disk"`} {
		if !strings.Contains(form, name) {
			t.Errorf("the functions form lost %s", name)
		}
	}
	// And the extraction really is one form: a control from the
	// persist-only form below must not appear in it, or the assertion above
	// would pass for a switch in the wrong place.
	if strings.Contains(form, `name="hid"`) {
		t.Fatal("the extracted markup spans more than the functions form; this test would not catch a misplaced switch")
	}
}

// The panel has to say which port the console is actually on: with the gadget
// console enabled it is the gadget's ttyGS, not the configured serial.device,
// and nothing else in the UI reports that.
func TestHardwarePanelShowsTheConsoleDevice(t *testing.T) {
	html := renderHardwarePanel(t, SettingsHardware{USBSerialConsole: true, ConsoleDevice: "/dev/ttyGS1"})

	if !strings.Contains(html, "/dev/ttyGS1") {
		t.Error("the hardware panel does not show the resolved console device")
	}
}

// f_acm costs two of the six IN endpoints the silicon serves, so the console
// composes only while the RHI NIC is off or CDC-EEM. Enabling it against a
// notify-carrying NIC is refused by the endpoint budget, and without that said
// here the operator's report is "the switch does nothing" — the one failure
// mode this copy exists to pre-empt. (The old vendor-specific gser warning is
// gone with gser: cdc_acm binds on class, so no manual usbserial bind is
// needed any more, and telling the operator to run one would be wrong.)
func TestHardwarePanelExplainsTheEndpointConstraint(t *testing.T) {
	html := renderHardwarePanel(t, SettingsHardware{USBSerialConsole: true, ConsoleDevice: "/dev/ttyGS0"})

	for _, want := range []string{"CDC-ACM", "ttyACM", "endpoint"} {
		if !strings.Contains(html, want) {
			t.Errorf("the serial console copy never mentions %q, so nothing tells the operator what the console is or when it will not compose", want)
		}
	}
	if strings.Contains(html, "usbserial") {
		t.Error("the copy still tells the operator to bind usbserial by hand; cdc_acm binds CDC-ACM on class")
	}
}

func renderSerialPanel(t *testing.T, m SettingsSerial) string {
	t.Helper()

	var sb strings.Builder
	if err := SettingsSerialBody(m).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// The Serial form configures a UART that the gadget console overrides wholesale.
// Editing baud rate there while the console is on a ttyGS changes nothing the
// operator can observe, so the form has to say which port is really in use and
// that these fields are not it.
func TestSerialPanelDisclosesTheGadgetConsoleOverride(t *testing.T) {
	overridden := renderSerialPanel(t, SettingsSerial{
		Device:              "/dev/ttyS1",
		ConsoleDevice:       "/dev/ttyGS0",
		GadgetConsoleActive: true,
	})
	if !strings.Contains(overridden, "/dev/ttyGS0") {
		t.Error("the serial panel does not show the effective console device")
	}
	if !strings.Contains(overridden, "USB Serial Console") {
		t.Error("the serial panel does not say the USB Serial Console is overriding this device")
	}

	// And it must not cry override when the configured UART really is the
	// console — that would send operators hunting a gadget they never enabled.
	plain := renderSerialPanel(t, SettingsSerial{
		Device:        "/dev/ttyS1",
		ConsoleDevice: "/dev/ttyS1",
	})
	if strings.Contains(plain, "USB Serial Console") {
		t.Error("the serial panel claims a gadget console override with the gadget console off")
	}
	if !strings.Contains(plain, "/dev/ttyS1") {
		t.Error("the serial panel does not show the console device when it is the configured UART")
	}
}
