// Package api composes the BMC's HTTP API. Each subpackage owns one
// sub-router (one route group of the surface): auth, application, vm,
// network, redfish, firmware and autoupdate. The web front-end lives in the
// ui package; telemetry middleware is registered by pkg/telemetry before
// any routes so it wraps them all.
package api

import (
	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/api/application"
	"github.com/pi-bmc/nanokvm-app/api/auth"
	"github.com/pi-bmc/nanokvm-app/api/autoupdate"
	"github.com/pi-bmc/nanokvm-app/api/firmware"
	"github.com/pi-bmc/nanokvm-app/api/network"
	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/api/vm"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/middleware"
)

// Register mounts every API sub-router on the engine. The authenticated
// group owns the /api prefix and its middleware chain in one place, so a
// subpackage cannot accidentally register an unauthenticated /api route;
// the few public endpoints (login, token check, the Redfish tree with its
// own session auth) take the engine explicitly.
func Register(r *gin.Engine, d *deps.Deps) {
	authed := r.Group("/api", middleware.CheckToken(logger.Or(d.Log).With("component", "http")))

	auth.Register(r, authed, d)
	application.Register(authed, d)
	vm.Register(authed, d)
	network.Register(authed, d)
	redfish.Register(r, d)
	firmware.Register(authed, d)
	autoupdate.Register(authed, d)
}
