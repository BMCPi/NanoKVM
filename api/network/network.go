package network

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

// handlers holds what every route handler in this package needs:
// process-lifetime dependencies (through d) and this package's component
// logger.
type handlers struct {
	d   *deps.Deps
	log *slog.Logger
}

// Register mounts the network routes on the shared authenticated group.
func Register(api *gin.RouterGroup, d *deps.Deps) {
	h := &handlers{d: d, log: logger.Or(d.Log).With("component", "api/network")}

	api.POST("/network/wol", h.WakeOnLAN)           // wake on lan
	api.GET("/network/wol/mac", h.GetMac)           // get mac list
	api.DELETE("/network/wol/mac", h.DeleteMac)     // delete mac
	api.POST("/network/wol/mac/name", h.SetMacName) // set mac name

	api.GET("/network/settings", h.GetSettings)      // get interface config
	api.PATCH("/network/settings", h.UpdateSettings) // patch config + re-apply
}
