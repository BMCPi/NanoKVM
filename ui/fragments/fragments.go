// Package fragments is the htmx surface of the UI: a route group under /ui that
// answers HTML fragments instead of the JSON envelope /api returns and the
// resources /redfish returns. Those two stay untouched — they are the
// device's public API and have consumers (gofish, bmclib, ipmitool, the
// Terraform provider) that must keep seeing JSON.
//
// A fragment handler gathers its data from pkg/* and the service layer the
// same way the JSON handlers do, then renders a templ partial from
// ui/components. The partials are the same functions the full-page render
// uses, so first paint and every subsequent swap come from one definition.
package fragments

import (
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"

	"github.com/a-h/templ"
	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/middleware"
	"github.com/pi-bmc/nanokvm-app/ui/render"
)

// handlers holds what a route handler in this package's log-touched files
// (firmware.go, media.go, overview.go, power.go, power_events.go,
// settings.go) needs: process-lifetime dependencies (through d) and the
// "ui" component logger, built once in Routes. bios.go and metrics.go have
// no log sites and keep taking *deps.Deps directly.
type handlers struct {
	d   *deps.Deps
	log *slog.Logger
}

// Routes mounts every /ui/... endpoint. It hangs off the group that
// has ResolveAuth applied and does its own auth rejection: RequireAuth's 302
// to /auth/login would be followed transparently by the browser and the
// login page swapped into whatever target the fragment was bound to.
func Routes(r *gin.RouterGroup, d *deps.Deps) {
	frag := r.Group("/ui")
	frag.Use(requireAuthFragment())

	h := &handlers{d: d, log: logger.Or(d.Log).With("component", "ui")}

	overviewFragmentRoutes(frag, h)
	powerFragmentRoutes(frag, h)
	settingsFragmentRoutes(frag, h)
	mediaFragmentRoutes(frag, h)
	metricsFragmentRoutes(frag, d)
	biosFragmentRoutes(frag, d)
	firmwareFragmentRoutes(frag, h)
}

// requireAuthFragment answers an unauthenticated fragment request with the
// HX-Redirect header, which htmx turns into a full-page navigation — the
// only way to leave a fragment swap and land on the login page.
func requireAuthFragment() gin.HandlerFunc {
	return func(c *gin.Context) {
		if middleware.IsAuthed(c) {
			c.Next()
			return
		}
		c.Header("HX-Redirect", "/auth/login")
		c.AbortWithStatus(http.StatusUnauthorized)
	}
}

// renderFragment writes a templ component as an HTML fragment response.
func renderFragment(c *gin.Context, component templ.Component) {
	c.Render(http.StatusOK, render.New(c.Request.Context(), http.StatusOK, component))
}

// hxToast raises a toast on the client after the response lands. The
// components.Toaster picks it up in the htmx:trigger handler wired by
// components.HTMXScript.
func hxToast(c *gin.Context, kind, title, description string) {
	appendTrigger(c, map[string]any{
		"ui-toast": map[string]string{
			"type":        kind,
			"title":       title,
			"description": description,
		},
	})
}

// appendTrigger merges into any HX-Trigger already set on the response, so a
// handler can both refresh siblings and raise a toast.
func appendTrigger(c *gin.Context, add map[string]any) {
	merged := map[string]any{}
	if existing := c.Writer.Header().Get("HX-Trigger"); existing != "" {
		// Anything we set is object form; a decode failure means someone set a
		// bare event name, which is still a valid HX-Trigger value.
		if err := json.Unmarshal([]byte(existing), &merged); err != nil {
			merged = map[string]any{existing: nil}
		}
	}
	maps.Copy(merged, add)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return
	}
	c.Header("HX-Trigger", string(encoded))
}
