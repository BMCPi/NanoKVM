package redfish

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/middleware"
)

// CheckAuth gates Redfish endpoints. It accepts (in order):
//
//  1. Authentication=disable in config — open passthrough.
//  2. An X-Auth-Token header or nano-kvm-token cookie (delegates to
//     middleware.CheckToken).
//  3. HTTP Basic Auth — standards-based Redfish clients (gofish, bmclib,
//     the Dell Terraform provider) fall back to Basic when they haven't
//     opened a session yet, and some skip sessions entirely.
//
// Requests from the USB host interface pass without credentials: DSP0270
// permits unauthenticated host-interface access, and the nftables isolation
// in pkg/network makes the source address trustworthy.
func CheckAuth(log *slog.Logger, svc *auth.Service) gin.HandlerFunc {
	tokenCheck := middleware.CheckToken(log)
	return func(c *gin.Context) {
		if IsHostInterfaceRequest(c) {
			c.Next()
			return
		}
		if config.GetInstance().Authentication == "disable" {
			c.Next()
			return
		}
		if user, pass, ok := c.Request.BasicAuth(); ok {
			if svc.ComparePlainAccount(user, pass) {
				c.Next()
				return
			}
			redfishErrorResponse(c, http.StatusUnauthorized, "invalid username or password")
			c.Abort()
			return
		}
		tokenCheck(c)
	}
}

// HostTrace logs every request that arrives over the USB host interface.
// The managed host's firmware is a Redfish client whose RELEASE builds
// carry no diagnostics of their own, so this log is the only record of
// what the firmware actually did during a boot — which resources its
// feature drivers walked, whether the pending Bios settings were read
// (GET /Systems/1/Bios/Settings) and consumed (DELETE of the same), and
// where an exchange stopped. LAN traffic is untouched.
func HostTrace(log *slog.Logger) gin.HandlerFunc {
	log = logger.Or(log)
	return func(c *gin.Context) {
		if !IsHostInterfaceRequest(c) {
			c.Next()
			return
		}
		c.Next()
		log.InfoContext(c.Request.Context(), "redfish host: request",
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()))
	}
}
