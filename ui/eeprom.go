package ui

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

// eepromPreviewHandler renders the EEPROM dialog's parsed-settings pane from
// the posted editor text. Serving it as a server-rendered fragment keeps the
// parse and default-filter hinting on the one Go implementation instead of a
// JS mirror in the dialog script.
func eepromPreviewHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Content string `json:"content"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.String(http.StatusBadRequest, "invalid request")
			return
		}
		settings := firmware.ParseEEPROMConfig(req.Content)
		render := newRender(c.Request.Context(), http.StatusOK, components.EEPROMPreview(settings))
		c.Render(http.StatusOK, render)
	}
}
