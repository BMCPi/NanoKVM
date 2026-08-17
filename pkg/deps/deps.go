// Package deps is the composition root's dependency carrier. cmd/server/main.go
// constructs every subsystem controller exactly once and wraps them in a
// *Deps, replacing the old GetController()/GetInstance() lazy-singleton
// pattern (each subsystem self-initializing behind a sync.Once on first
// access, invisible to callers and impossible to substitute in tests).
//
// Two consumers need it:
//   - Gin handler code (api/*, ui/*.go): receives *Deps as a constructor
//     argument when its Service/route-group is built, and closes over it.
//   - templ view code (ui/components/*.templ): templ.Component.Render(ctx, w)
//     propagates ctx down through every nested component automatically, so
//     Middleware below attaches Deps once per request and any component can
//     reach it via FromContext(ctx) without threading it through every
//     intermediate layout/page parameter list.
package deps

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/hid"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/video/rtc"
)

// Deps holds the process-wide subsystem controllers built once at startup.
type Deps struct {
	Config   *config.Config
	Power    *power.Controller
	Firmware *firmware.Controller

	// Video is the WebRTC hub over the HDMI capture pipeline. It is nil on a
	// board with no capture hardware, or when the pipeline failed to
	// initialize -- the rest of the BMC (Redfish, IPMI, the serial console)
	// has to keep working in that case, so handlers check for nil rather
	// than the server refusing to start.
	Video *rtc.Hub

	// HID drives the USB keyboard/mouse gadget for the HDMI console. Nil when
	// the gadget's HID functions are not configured, in which case input
	// handlers report that rather than the routes vanishing.
	HID *hid.Controller
}

type ctxKey struct{}

// Middleware attaches d to every request's context. Install it before any
// route that (transitively) reads FromContext — Register in both api and ui
// does this first.
func Middleware(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxKey{}, d))
		c.Next()
	}
}

// FromContext returns the Deps installed by Middleware. Panics if absent:
// every request path installs Middleware before any handler that calls this,
// so a miss means a route was wired outside Register.
func FromContext(ctx context.Context) *Deps {
	d, ok := ctx.Value(ctxKey{}).(*Deps)
	if !ok {
		panic("deps: no Deps in request context — Middleware not installed on this route")
	}
	return d
}
