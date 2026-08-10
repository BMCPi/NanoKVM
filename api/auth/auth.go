// Package auth exposes the authentication API: login/logout, account and
// password management, and the token validation endpoint the web UI uses for
// redirect decisions.
package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/middleware"
)

// Register mounts the public auth routes on the engine and the
// token-protected ones on the shared /api group.
func Register(r *gin.Engine, api *gin.RouterGroup) {
	service := NewService()

	r.POST("/api/auth/login", service.Login) // login

	// Token validation for client-side redirect decisions. ResolveAuth owns
	// the cookie/JWT logic (and the authentication=disable bypass); this
	// handler only reports the outcome.
	r.GET("/api/auth/check", middleware.ResolveAuth(), func(c *gin.Context) {
		if middleware.IsAuthed(c) {
			c.JSON(http.StatusOK, gin.H{"valid": true})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"valid": false})
	})

	api.GET("/auth/password", service.IsPasswordUpdated) // is password updated
	api.GET("/auth/account", service.GetAccount)         // get account
	api.POST("/auth/password", service.ChangePassword)   // change password
	api.POST("/auth/logout", service.Logout)             // logout
}
