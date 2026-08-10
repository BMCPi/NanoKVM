package ui

// fragments_power.go serves the navbar power menu's actions. The pill itself
// is driven by the GPIO SSE stream (see api/vm.StreamGpio), not htmx, so a
// handler here only needs to kick the controller and toast the result —
// there is no fragment to re-render.

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

// powerActionLabels turns an action into the past-tense label used in the
// success toast, e.g. "on" -> "Power on".
var powerActionLabels = map[string]string{
	"on":       "Power on",
	"off":      "Power off",
	"reset":    "Reset",
	"forceoff": "Force off",
	"rpiboot":  "Recovery (rpiboot)",
}

func powerFragmentRoutes(g *gin.RouterGroup) {
	p := g.Group("/power")
	p.POST("/:action", postPowerAction)
}

func postPowerAction(c *gin.Context) {
	action := c.Param("action")
	label, ok := powerActionLabels[action]
	if !ok {
		hxToast(c, "error", "Power action failed", fmt.Sprintf("unknown action %q", action))
		c.Status(http.StatusBadRequest)
		return
	}

	ctrl := power.GetController()
	var err error
	switch action {
	case "on":
		err = ctrl.PowerOn()
	case "off":
		err = ctrl.PowerOff()
	case "forceoff":
		err = ctrl.ForceOff()
	case "reset":
		err = ctrl.Reset()
	case "rpiboot":
		err = ctrl.Rpiboot()
	}

	if err != nil {
		log.Errorf("ui: power action %s failed: %v", action, err)
		hxToast(c, "error", label+" failed", err.Error())
		c.Status(http.StatusConflict)
		return
	}

	hxToast(c, "success", label, "")
	c.Status(http.StatusOK)
}
