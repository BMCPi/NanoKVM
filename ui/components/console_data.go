package components

import (
	"strconv"
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// ConsoleModel is what the serial pane's header says about the port the web
// terminal is on. Device is resolved, not configured: the serial broker
// prefers the USB gadget's ttyGS over serial.device at open time (see
// serial.ConsoleDeviceInfo), so the header follows the broker or it lies.
type ConsoleModel struct {
	// Device is the tty the broker opens, e.g. /dev/ttyS1 or /dev/ttyGS0.
	Device string
	// FromGadget reports that Device is the USB CDC-ACM console rather than
	// the configured UART. Framing is then empty: baud rate and framing are
	// a UART's and mean nothing on a gadget port.
	FromGadget bool
	// Framing is the UART's line settings in the usual shorthand, "115200 8N1".
	Framing string
}

// NewConsoleModel describes the console port. device and fromGadget are the
// broker's answer (serial.ConsoleDeviceInfo); s is the configured UART.
func NewConsoleModel(device string, fromGadget bool, s config.Serial) ConsoleModel {
	m := ConsoleModel{Device: device, FromGadget: fromGadget}
	if !fromGadget {
		m.Framing = serialFraming(s)
	}
	return m
}

// serialFraming renders "115200 8N1" from the port settings: baud rate, data
// bits, the parity's initial, stop bits. Parity is stored lower-case by the
// settings form and capitalised by Redfish, so it is matched either way.
func serialFraming(s config.Serial) string {
	parity := "N"
	if p := strings.ToLower(s.Parity); p != "" && p != "none" {
		parity = strings.ToUpper(p[:1])
	}
	return strconv.Itoa(s.BaudRate) + " " + strconv.Itoa(s.DataBits) + parity + strconv.Itoa(s.StopBits)
}

// Detail is the text beside the device: the UART's framing, or which kind of
// port the gadget console is.
func (m ConsoleModel) Detail() string {
	if m.FromGadget {
		return "USB CDC-ACM"
	}
	return m.Framing
}
