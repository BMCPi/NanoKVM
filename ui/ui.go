// Package ui owns the web front-end of the BMC: the templ layouts, pages and
// components (including the vendored shadcn-templ component library), the
// embedded static assets, and the gin wiring that serves them.
package ui

import (
	"io/fs"
	"net/http"

	"github.com/pi-bmc/nanokvm-app/server/middleware"
	"github.com/pi-bmc/nanokvm-app/ui/assets"
	"github.com/pi-bmc/nanokvm-app/ui/components"
	"github.com/pi-bmc/nanokvm-app/ui/pages"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Register installs the UI onto the gin engine: the templ HTML renderer
// (falling back to gin's default for non-templ renders), the embedded static
// assets, the component JS bundle, and every page route. API routes stay in
// server/router.
func Register(r *gin.Engine) {
	// Configure templ renderer with fallback to Gin's default HTML renderer.
	r.HTMLRender = &HTMLTemplRenderer{FallbackHTMLRenderer: r.HTMLRender}

	staticRoutes(r)
	pageRoutes(r)
}

// staticRoutes serves the embedded static assets and the component JS bundle.
func staticRoutes(r *gin.Engine) {
	cssFS, _ := fs.Sub(assets.CSS, "css")
	jsFS, _ := fs.Sub(assets.JS, "js")
	imgFS, _ := fs.Sub(assets.Img, "img")

	r.StaticFS("/css", http.FS(cssFS))
	r.StaticFS("/js", http.FS(jsFS))
	r.StaticFS("/img", http.FS(imgFS))

	// Component JS bundle rendered into the layout by components.Scripts().
	r.GET("/components/*filepath", gin.WrapH(components.ScriptsHandler()))

	// Favicon shortcut
	r.GET("/favicon.ico", func(c *gin.Context) {
		data, err := assets.Img.ReadFile("img/favicon.ico")
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Data(http.StatusOK, "image/x-icon", data)
	})
}

// pageRoutes registers every templ-rendered page.
func pageRoutes(r *gin.Engine) {
	// Public auth pages (no middleware)
	r.GET("/auth/login", func(c *gin.Context) {
		render := newRender(c.Request.Context(), http.StatusOK, pages.LoginPage())
		c.Render(http.StatusOK, render)
	})

	// All page routes resolve auth status (sets authed flag, never redirects).
	pageGroup := r.Group("/")
	pageGroup.Use(middleware.ResolveAuth())

	// Password reset is reachable both logged-in and as a guest.
	pageGroup.GET("/auth/password", func(c *gin.Context) {
		render := newRender(c.Request.Context(), http.StatusOK, pages.PasswordPage(middleware.IsAuthed(c)))
		c.Render(http.StatusOK, render)
	})

	// Protected pages — require valid JWT cookie, redirect to login otherwise.
	protected := pageGroup.Group("/")
	protected.Use(middleware.RequireAuth())

	protected.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/dashboard")
	})
	protected.GET("/dashboard", func(c *gin.Context) {
		render := newRender(c.Request.Context(), http.StatusOK, pages.Home())
		c.Render(http.StatusOK, render)
	})
	protected.GET("/console", func(c *gin.Context) {
		render := newRender(c.Request.Context(), http.StatusOK, pages.ConsolePage())
		c.Render(http.StatusOK, render)
	})
	protected.GET("/settings", func(c *gin.Context) {
		render := newRender(c.Request.Context(), http.StatusOK, pages.SettingsPage())
		c.Render(http.StatusOK, render)
	})

	// API docs — custom templ-rendered view of the embedded OpenAPI
	// spec. The raw spec stays public at /redfish/v1/openapi.{yaml,json}
	// for tooling discovery; the rendered docs page is behind auth so
	// it shares the dashboard chrome.
	protected.GET("/docs", apiDocsHandler())
}

// apiDocsHandler parses the OpenAPI spec once (sync.Once via the model
// cache in apidocs.go) and renders the pages.APIDocsPage on every request.
func apiDocsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		model, err := loadAPIDocsModel()
		if err != nil {
			log.Errorf("api docs: load model: %v", err)
			c.String(http.StatusInternalServerError, "API docs unavailable: %v", err)
			return
		}
		render := newRender(c.Request.Context(), http.StatusOK, pages.APIDocsPage(model))
		c.Render(http.StatusOK, render)
	}
}
