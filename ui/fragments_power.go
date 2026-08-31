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

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

// powerActionLabels turns an action into the past-tense label used in the
// success toast, e.g. "on" -> "Power on".
var powerActionLabels = map[string]string{
	"on":       "Power on",
	"off":      "Power off",
	"reset":    "Reset",
	"forceoff": "Force off",
}

func powerFragmentRoutes(g *gin.RouterGroup, d *deps.Deps) {
	p := g.Group("/power")
	// The boot-override section is fetched when the menu opens so it shows
	// what is actually staged — that state also moves from the overview
	// card, the Redfish API and other sessions. Registered before the
	// wildcard, and on GET where the action route (POST) cannot shadow it.
	p.GET("/boot-override", getPowerBootOverride(d))
	// The SSE fragment stream lives in fragments_power_events.go: it answers
	// a different shape of request (long-lived, text/event-stream) from the
	// rest of this file's short htmx POSTs, but it is registered alongside
	// them because it is part of the same navbar power surface.
	p.GET("/events", getPowerEvents(d))
	p.POST("/:action", postPowerAction(d))
}

// getPowerBootOverride renders the power menu's boot-override section from
// the same staged state the overview card reads — the Boot block of the
// Redfish system inventory, which is what PATCH /redfish/v1/Systems/1 writes
// and the host firmware applies at its next boot.
func getPowerBootOverride(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		renderFragment(c, components.PowerBootOverride(
			stagedBootOverride(c.Request.Context(), d)))
	}
}

func postPowerAction(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		action := c.Param("action")
		label, ok := powerActionLabels[action]
		if !ok {
			hxToast(c, "error", "Power action failed", fmt.Sprintf("unknown action %q", action))
			c.Status(http.StatusBadRequest)
			return
		}

		ctrl := d.Power

		// Detached from the request: htmx aborts the in-flight fetch when the
		// user navigates away, and that must not abandon a reset between its
		// off and on phases. See deps.ActionContext.
		ctx, cancel := d.ActionContext(power.ActionTimeout)
		defer cancel()

		var err error
		switch action {
		case "on":
			err = ctrl.PowerOn(ctx)
		case "off":
			err = ctrl.PowerOff(ctx)
		case "forceoff":
			err = ctrl.ForceOff(ctx)
		case "reset":
			err = ctrl.Reset(ctx)
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
}
