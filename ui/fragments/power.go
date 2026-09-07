package fragments

// fragments_power.go serves the navbar power menu's actions. The pill and
// the toggle are driven by GET /ui/power/events (fragments_power_events.go)
// via htmx's SSE extension, so a handler here only needs to kick the
// controller and toast the result — the transition itself reaches the
// client over the SSE stream, not this response.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

// powerAction is one /ui/power/:action — the past-tense label for its toast,
// e.g. "Power on", and the controller method that services it. One table
// rather than a label map beside a switch, so an action cannot be known to
// one and not the other, which would toast success for a request that did
// nothing.
type powerAction struct {
	label string
	run   func(*power.Controller, context.Context) error
}

// powerActions maps the menu's buttons to the controller. reset and
// forcereset are the two resets of the board-agnostic design's §1 table:
// reset follows the operator's power.reset policy (Restart — the dedicated
// line where wired, as Redfish ForceRestart and IPMI hard reset do), while
// forcereset is the unconditional force-off+repower (Reset — Redfish
// PowerCycle, IPMI power cycle).
var powerActions = map[string]powerAction{
	"on":         {label: "Power on", run: (*power.Controller).PowerOn},
	"off":        {label: "Power off", run: (*power.Controller).PowerOff},
	"reset":      {label: "Reset", run: (*power.Controller).Restart},
	"forceoff":   {label: "Force off", run: (*power.Controller).ForceOff},
	"forcereset": {label: "Force reset", run: (*power.Controller).Reset},
}

func powerFragmentRoutes(g *gin.RouterGroup, h *handlers) {
	p := g.Group("/power")
	// The boot-override section is fetched when the menu opens so it shows
	// what is actually staged — that state also moves from the overview
	// card, the Redfish API and other sessions. Registered before the
	// wildcard, and on GET where the action route (POST) cannot shadow it.
	p.GET("/boot-override", h.getPowerBootOverride)
	// The SSE fragment stream lives in fragments_power_events.go: it answers
	// a different shape of request (long-lived, text/event-stream) from the
	// rest of this file's short htmx POSTs, but it is registered alongside
	// them because it is part of the same navbar power surface.
	p.GET("/events", h.getPowerEvents)
	p.POST("/:action", h.postPowerAction)
}

// getPowerBootOverride renders the power menu's boot-override section from
// the same staged state the Host Firmware card reads — the Boot block of the
// Redfish system inventory, which is what PATCH /redfish/v1/Systems/1 writes
// and the host firmware applies at its next boot.
func (h *handlers) getPowerBootOverride(c *gin.Context) {
	renderFragment(c, components.PowerBootOverride(
		stagedBootOverride(c.Request.Context(), h.d)))
}

func (h *handlers) postPowerAction(c *gin.Context) {
	action := c.Param("action")
	a, ok := powerActions[action]
	if !ok {
		hxToast(c, "error", "Power action failed", fmt.Sprintf("unknown action %q", action))
		c.Status(http.StatusBadRequest)
		return
	}

	// Detached from the request: htmx aborts the in-flight fetch when the
	// user navigates away, and that must not abandon a reset between its
	// off and on phases. See deps.ActionContext.
	ctx, cancel := h.d.ActionContext(power.ActionTimeout)
	defer cancel()

	if err := a.run(h.d.Power, ctx); err != nil {
		h.log.ErrorContext(c.Request.Context(), "ui: power action failed", slog.String("action", action), slog.Any("err", err))
		msg := err.Error()
		if errors.Is(err, power.ErrNoResetLine) {
			// power.reset is "line" but the board wires no reset pin — an
			// actionable message rather than the raw sentinel text, and no
			// silent power cycle substituted: Force reset is the button for
			// that.
			msg = "reset line not wired; use Force reset, or set power.reset to auto or cycle"
		}
		hxToast(c, "error", a.label+" failed", msg)
		c.Status(http.StatusConflict)
		return
	}

	hxToast(c, "success", a.label, "")
	c.Status(http.StatusOK)
}
