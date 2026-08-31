package application

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

// Register mounts the application routes on the shared authenticated group.
func Register(api *gin.RouterGroup, d *deps.Deps) {
	h := &handlers{d: d, log: logger.Or(d.Log).With("component", "api/application")}

	api.GET("/application/version", h.GetVersion)            // get application version
	api.POST("/application/update", h.Update)                // update application
	api.POST("/application/update/offline", h.OfflineUpdate) // update application offline
}
