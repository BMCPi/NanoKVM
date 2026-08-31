package vm

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

// errorKey is the JSON field every failure response in this package uses.
const errorKey = "error"

// handlers holds what every route handler in this package needs: the
// subsystem controllers and process-lifetime context (through d) and this
// package's component logger.
type handlers struct {
	d   *deps.Deps
	log *slog.Logger
}

// Register mounts the vm routes on the shared authenticated group.
func Register(api *gin.RouterGroup, d *deps.Deps) {
	h := &handlers{d: d, log: logger.Or(d.Log).With("component", "api/vm")}

	api.GET("/vm/info", h.GetInfo)         // get device information
	api.GET("/vm/hardware", h.GetHardware) // get hardware version

	api.POST("/vm/gpio", h.SetGpio)          // update gpio
	api.GET("/vm/gpio", h.GetGpio)           // get gpio
	api.GET("/vm/gpio/events", h.StreamGpio) // stream power state (SSE)

	api.GET("/vm/video", h.Video) // WebRTC signaling for the HDMI console

	api.GET("/vm/hid", h.HID) // keyboard/mouse input for the HDMI console (WebSocket)

	api.GET("/vm/macros", h.GetMacros)          // list keyboard macros
	api.POST("/vm/macros", h.CreateMacro)       // create a keyboard macro
	api.PUT("/vm/macros/:id", h.UpdateMacro)    // update a keyboard macro
	api.DELETE("/vm/macros/:id", h.DeleteMacro) // delete a keyboard macro
	api.POST("/vm/macros/:id/run", h.RunMacro)  // send a macro to the host

	api.GET("/vm/terminal", h.Terminal)                // web terminal (host serial console)
	api.GET("/vm/terminal/capture", h.TerminalCapture) // persisted serial capture (host boot logs)
	api.GET("/vm/shell", h.Shell)                      // web terminal (BMC shell)

	api.GET("/vm/device/virtual", h.GetVirtualDevice)     // get virtual device
	api.POST("/vm/device/virtual", h.UpdateVirtualDevice) // update virtual device

	api.GET("/vm/ssh", h.GetSSHState)         // get SSH state
	api.POST("/vm/ssh/enable", h.EnableSSH)   // enable SSH
	api.POST("/vm/ssh/disable", h.DisableSSH) // disable SSH
	api.GET("/vm/ssh/keys", h.GetSSHKeys)     // get authorized_keys
	api.POST("/vm/ssh/keys", h.SetSSHKeys)    // set authorized_keys

	api.POST("/vm/tls", h.SetTLS) // enable/disable TLS

	api.POST("/vm/system/reboot", h.Reboot) // reboot system
}
