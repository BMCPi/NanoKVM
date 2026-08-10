package vm

import (
	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/vm"
)

// Register mounts the vm routes on the shared authenticated group.
func Register(api *gin.RouterGroup) {
	service := vm.NewService()

	api.GET("/vm/info", service.GetInfo)         // get device information
	api.GET("/vm/hardware", service.GetHardware) // get hardware version

	api.POST("/vm/gpio", service.SetGpio)          // update gpio
	api.GET("/vm/gpio", service.GetGpio)           // get gpio
	api.GET("/vm/gpio/events", service.StreamGpio) // stream power state (SSE)

	api.GET("/vm/terminal", service.Terminal)                // web terminal (host serial console)
	api.GET("/vm/terminal/capture", service.TerminalCapture) // persisted serial capture (host boot logs)
	api.GET("/vm/shell", service.Shell)                      // web terminal (BMC shell)

	api.GET("/vm/device/virtual", service.GetVirtualDevice)     // get virtual device
	api.POST("/vm/device/virtual", service.UpdateVirtualDevice) // update virtual device

	api.GET("/vm/ssh", service.GetSSHState)         // get SSH state
	api.POST("/vm/ssh/enable", service.EnableSSH)   // enable SSH
	api.POST("/vm/ssh/disable", service.DisableSSH) // disable SSH
	api.GET("/vm/ssh/keys", service.GetSSHKeys)     // get authorized_keys
	api.POST("/vm/ssh/keys", service.SetSSHKeys)    // set authorized_keys

	api.POST("/vm/tls", service.SetTls) // enable/disable TLS

	api.POST("/vm/system/reboot", service.Reboot) // reboot system
}
