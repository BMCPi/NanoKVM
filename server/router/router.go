package router

import (
	"io/fs"
	"net/http"

	"github.com/pi-bmc/nanokvm-app/server/assets"
	"github.com/pi-bmc/nanokvm-app/server/components"
	"github.com/pi-bmc/nanokvm-app/server/config"
	"github.com/pi-bmc/nanokvm-app/server/middleware"
	"github.com/pi-bmc/nanokvm-app/server/pages"
	"github.com/pi-bmc/nanokvm-app/server/telemetry"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

func Init(r *gin.Engine) {
	telemetryRoutes(r)
	web(r)
	server(r)
	log.Debugf("router init done")
}

// telemetryRoutes wires up the otelgin middleware (when enabled) and the
// Prometheus scrape endpoint. Must run before any handlers are registered
// so the middleware wraps them all.
func telemetryRoutes(r *gin.Engine) {
	tcfg := config.GetInstance().Telemetry
	if !tcfg.Enabled {
		return
	}

	r.Use(otelgin.Middleware(tcfg.ServiceName))

	if tcfg.Prometheus.Enabled {
		path := tcfg.Prometheus.Path
		if path == "" {
			path = "/metrics"
		}
		handler := promhttp.HandlerFor(telemetry.PromRegistry, promhttp.HandlerOpts{
			Registry: telemetry.PromRegistry,
		})
		r.GET(path, gin.WrapH(handler))
		log.Infof("telemetry: prometheus exposed at %s", path)
	}
}

func web(r *gin.Engine) {
	// Serve embedded static assets
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

	// Public auth pages (no middleware)
	r.GET("/auth/login", func(c *gin.Context) {
		render := newRender(c.Request.Context(), http.StatusOK, pages.LoginPage())
		c.Render(http.StatusOK, render)
	})

	// Token validation endpoint for client-side redirect decisions
	r.GET("/api/auth/check", func(c *gin.Context) {
		conf := config.GetInstance()
		if conf.Authentication == "disable" {
			c.JSON(http.StatusOK, gin.H{"valid": true})
			return
		}
		cookie, err := c.Cookie("nano-kvm-token")
		if err != nil || cookie == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"valid": false})
			return
		}
		if _, err := middleware.ParseJWT(cookie); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"valid": false})
			return
		}
		c.JSON(http.StatusOK, gin.H{"valid": true})
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
// cache below) and renders the pages.APIDocsPage on every request.
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

func server(r *gin.Engine) {
	authRouter(r)
	applicationRouter(r)
	vmRouter(r)
	networkRouter(r)
	redfishRouter(r)
	firmwareRouter(r)
	autoUpdateRouter(r)
}
