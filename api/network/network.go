package network

import (
	"github.com/gin-gonic/gin"
)

// Register mounts the network routes on the shared authenticated group.
func Register(api *gin.RouterGroup) {
	service := NewService()

	api.POST("/network/wol", service.WakeOnLAN)           // wake on lan
	api.GET("/network/wol/mac", service.GetMac)           // get mac list
	api.DELETE("/network/wol/mac", service.DeleteMac)     // delete mac
	api.POST("/network/wol/mac/name", service.SetMacName) // set mac name

	api.GET("/network/settings", service.GetSettings)      // get interface config
	api.PATCH("/network/settings", service.UpdateSettings) // patch config + re-apply
}
