package fragments

// console.go serves the dashboard's serial console header. The header names
// the port the terminal is on, and that port moves at runtime — patchSerial
// changes serial.device, the USB serial console toggle swaps in the gadget's
// ttyGS — from a settings dialog on the same page, so the label is a fragment
// that re-fetches on the console-changed event those handlers raise rather
// than a value baked in at page render.

import (
	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/device/serial"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func consoleFragmentRoutes(g *gin.RouterGroup) {
	g.GET("/console/device", func(c *gin.Context) {
		renderFragment(c, components.ConsoleDeviceLabel(CurrentConsole()))
	})
}

// CurrentConsole is the header's view of the console port, from the same
// resolution the broker uses at open time: the gadget's ttyGS when the USB
// serial console is on, else the configured serial.device with its framing.
// Exported for the dashboard page, which paints the same label at load.
func CurrentConsole() components.ConsoleModel {
	device, fromGadget := serial.ConsoleDeviceInfo()
	return components.NewConsoleModel(device, fromGadget, config.GetInstance().Serial)
}
