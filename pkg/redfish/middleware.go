package redfish

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
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
func CheckAuth() gin.HandlerFunc {
	tokenCheck := middleware.CheckToken()
	return func(c *gin.Context) {
		if config.GetInstance().Authentication == "disable" {
			c.Next()
			return
		}
		if user, pass, ok := c.Request.BasicAuth(); ok {
			if auth.ComparePlainAccount(user, pass) {
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
