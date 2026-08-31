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
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/hid"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/video/rtc"
)

// Deps holds the process-wide subsystem controllers built once at startup.
type Deps struct {
	// Ctx is the process-lifetime context: the root context from main,
	// cancelled when the server starts shutting down.
	//
	// It is NOT a substitute for the request context. Handlers pass
	// c.Request.Context() to anything whose result the client is waiting for,
	// so a disconnect stops the work. Ctx is for the other case — work a
	// handler starts that must outlive the request but must still stop at
	// shutdown:
	//
	//   - Detached goroutines (capsule staging, media fetch). These already
	//     had to avoid the request context, and previously reached for
	//     context.Background(), which meant SIGTERM abandoned a half-written
	//     capsule with nothing watching.
	//   - Side-effecting operations that are worse when interrupted than when
	//     completed. A power reset is off-then-on; abandoning it midway
	//     because a browser tab closed leaves the host powered down. See
	//     ActionContext.
	//
	// Nil in tests that construct Deps directly; ActionContext handles that.
	Ctx context.Context

	// Log is the root injected logger, set once by main from logger.Init's
	// return. Handler packages derive their component logger from it inside
	// Register (never at package init — see the slog-DI spec's invariant).
	Log *slog.Logger

	Config   *config.Config
	Power    *power.Controller
	Firmware *firmware.Controller

	// Auth is the credential store and brute-force guard shared by the web
	// login form, the Redfish session/Basic-Auth paths, and (outside the
	// request path) the SSH server — all of them must see the same instance
	// so a lockout on one login surface applies to the others. auth is a
	// library, not a component: main constructs it untagged and every caller
	// applies its own component-tagged logger elsewhere in its own chain.
	Auth *auth.Service

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

// ActionContext returns a context for a side-effecting operation the caller
// must not abandon just because the client stopped listening, bounded by
// timeout, plus its cancel func.
//
// It derives from Ctx, not from the request, so the operation survives a
// closed browser tab but is still cancelled at shutdown. Falls back to
// context.Background when Ctx is unset, which is only the case in tests that
// build a Deps by hand.
func (d *Deps) ActionContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	parent := d.Ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}
