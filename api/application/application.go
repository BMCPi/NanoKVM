package application

import (
	"github.com/pi-bmc/nanokvm-app/pkg/application"

	"github.com/gin-gonic/gin"
)

// Register mounts the application routes on the shared authenticated group.
func Register(api *gin.RouterGroup) {
	service := application.NewService()

	api.GET("/application/version", service.GetVersion)            // get application version
	api.POST("/application/update", service.Update)                // update application
	api.POST("/application/update/offline", service.OfflineUpdate) // update application offline
}
