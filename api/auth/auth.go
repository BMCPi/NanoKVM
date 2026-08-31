// Package auth exposes the authentication API: login/logout, account and
// password management, and the token validation endpoint the web UI uses for
// redirect decisions.
package auth

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/middleware"
)

// handlers holds what every route handler in this package needs:
// process-lifetime dependencies (through d) and this package's component
// logger.
type handlers struct {
	d   *deps.Deps
	log *slog.Logger
}

// Register mounts the public auth routes on the engine and the
// token-protected ones on the shared /api group.
func Register(r *gin.Engine, api *gin.RouterGroup, d *deps.Deps) {
	h := &handlers{d: d, log: logger.Or(d.Log).With("component", "api/auth")}

	r.POST("/api/auth/login", h.Login) // login

	// Token validation for client-side redirect decisions. ResolveAuth owns
	// the cookie/JWT logic (and the authentication=disable bypass); this
	// handler only reports the outcome.
	r.GET("/api/auth/check", middleware.ResolveAuth(h.log), func(c *gin.Context) {
		if middleware.IsAuthed(c) {
			c.JSON(http.StatusOK, gin.H{"valid": true})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"valid": false})
	})

	api.GET("/auth/password", h.IsPasswordUpdated) // is password updated
	api.GET("/auth/account", h.GetAccount)         // get account
	api.POST("/auth/password", h.ChangePassword)   // change password
	api.POST("/auth/logout", h.Logout)             // logout
}
