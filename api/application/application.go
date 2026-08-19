package application

import (
	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
)

// Register mounts the application routes on the shared authenticated group.
func Register(api *gin.RouterGroup, d *deps.Deps) {
	service := NewService(d)

	api.GET("/application/version", service.GetVersion)            // get application version
	api.POST("/application/update", service.Update)                // update application
	api.POST("/application/update/offline", service.OfflineUpdate) // update application offline
}
