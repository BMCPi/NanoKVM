package router

import (
	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/server/middleware"
	"github.com/pi-bmc/nanokvm-app/server/service/network"
)

func networkRouter(r *gin.Engine) {
	service := network.NewService()

	api := r.Group("/api").Use(middleware.CheckToken())

	api.POST("/network/wol", service.WakeOnLAN)           // wake on lan
	api.GET("/network/wol/mac", service.GetMac)           // get mac list
	api.DELETE("/network/wol/mac", service.DeleteMac)     // delete mac
	api.POST("/network/wol/mac/name", service.SetMacName) // set mac name

	api.GET("/network/settings", service.GetSettings)      // get interface config
	api.PATCH("/network/settings", service.UpdateSettings) // patch config + re-apply
}
