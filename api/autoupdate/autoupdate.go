package autoupdate

import (
	"net/http"

	"github.com/pi-bmc/nanokvm-app/pkg/autoupdate"
	"github.com/pi-bmc/nanokvm-app/pkg/config"

	"github.com/gin-gonic/gin"
)

// Register wires the settings GET/PATCH endpoints used by the settings
// dialog onto the shared authenticated group. Changes persist to
// /etc/kvm/server.yaml and restart the background ticker so toggles take
// effect immediately.
func Register(api *gin.RouterGroup) {
	api.GET("/autoupdate/settings", func(c *gin.Context) {
		c.JSON(http.StatusOK, config.GetInstance().AutoUpdate)
	})

	api.PATCH("/autoupdate/settings", func(c *gin.Context) {
		var req struct {
			Enabled         *bool `json:"enabled"`
			IntervalMinutes *int  `json:"intervalMinutes"`
			Application     *bool `json:"application"`
			BIOS            *bool `json:"bios"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		cfg := &config.GetInstance().AutoUpdate
		if req.Enabled != nil {
			cfg.Enabled = *req.Enabled
		}
		if req.IntervalMinutes != nil && *req.IntervalMinutes > 0 {
			cfg.IntervalMinutes = *req.IntervalMinutes
		}
		if req.Application != nil {
			cfg.Application = *req.Application
		}
		if req.BIOS != nil {
			cfg.BIOS = *req.BIOS
		}

		config.Save()
		autoupdate.Start() // re-reads config; cancels existing ticker if running

		c.JSON(http.StatusOK, cfg)
	})
}
