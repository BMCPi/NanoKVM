// Package ui owns the web front-end of the BMC: the templ layouts, pages and
// components (including the vendored shadcn-templ component library), the
// embedded static assets, and the gin wiring that serves them.
package ui

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/middleware"
	"github.com/pi-bmc/nanokvm-app/ui/assets"
	"github.com/pi-bmc/nanokvm-app/ui/components"
	"github.com/pi-bmc/nanokvm-app/ui/fragments"
	"github.com/pi-bmc/nanokvm-app/ui/pages"
	"github.com/pi-bmc/nanokvm-app/ui/render"

	"github.com/gin-gonic/gin"
)

// Register installs the UI onto the gin engine: the templ HTML renderer
// (falling back to gin's default for non-templ renders), the embedded static
// assets, the component JS bundle, and every page route. API routes stay in
// the api package.
func Register(r *gin.Engine, d *deps.Deps) {
	r.Use(deps.Middleware(d))
	staticRoutes(r)
	pageRoutes(r, d)
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

	// Favicon shortcut for bare browser probes (pages link /img/favicon.ico).
	// Read once — embed.FS.ReadFile copies, so a per-request read would
	// allocate the icon on every hit.
	favicon, _ := assets.Img.ReadFile("img/favicon.ico")
	r.GET("/favicon.ico", func(c *gin.Context) {
		if favicon == nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Data(http.StatusOK, "image/x-icon", favicon)
	})
}

// pageRoutes registers every templ-rendered page.
func pageRoutes(r *gin.Engine, d *deps.Deps) {
	// Public auth page. ResolveAuth only flags the session; an already
	// authed visitor is bounced to the dashboard server-side instead of
	// via a client-side /api/auth/check probe.
	r.GET("/auth/login", middleware.ResolveAuth(), func(c *gin.Context) {
		if middleware.IsAuthed(c) {
			c.Redirect(http.StatusFound, "/dashboard")
			return
		}
		c.Render(http.StatusOK, render.New(c.Request.Context(), http.StatusOK, pages.LoginPage()))
	})

	// All page routes resolve auth status (sets authed flag, never redirects).
	pageGroup := r.Group("/")
	pageGroup.Use(middleware.ResolveAuth())

	// htmx fragment endpoints. Registered on pageGroup rather than protected
	// so they can reject with HX-Redirect instead of RequireAuth's 302, which
	// htmx would follow and swap the login page into the fragment's target.
	fragments.Routes(pageGroup, d)

	// Password reset is reachable both logged-in and as a guest.
	pageGroup.GET("/auth/password", func(c *gin.Context) {
		c.Render(http.StatusOK, render.New(c.Request.Context(), http.StatusOK, pages.PasswordPage(middleware.IsAuthed(c))))
	})

	// Logout for the navbar's hx-post. On pageGroup rather than the fragment
	// group: logging out must succeed even with an expired token, where
	// requireAuthFragment would reject before the cookie could be cleared.
	// Mirrors POST /api/auth/logout, plus the cookie handling and redirect the
	// browser side used to do in JavaScript.
	pageGroup.POST("/ui/logout", func(c *gin.Context) {
		if config.GetInstance().JWT.RevokeTokensOnLogout {
			config.RegenerateSecretKey()
		}
		middleware.ClearAuthCookie(c)
		c.Header("HX-Redirect", "/auth/login")
		c.Status(http.StatusOK)
	})

	// Protected pages — require valid JWT cookie, redirect to login otherwise.
	protected := pageGroup.Group("/")
	protected.Use(middleware.RequireAuth())

	protected.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/dashboard")
	})
	protected.GET("/dashboard", func(c *gin.Context) {
		c.Render(http.StatusOK, render.New(c.Request.Context(), http.StatusOK, pages.Home(homeModel())))
	})
	// Legacy routes: the serial console and settings now live on the
	// dashboard, so these redirect server-side for old links/bookmarks.
	protected.GET("/console", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/dashboard")
	})
	protected.GET("/settings", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/dashboard")
	})
	// BIOS configuration is a dialog on the dashboard, not its own page.
	protected.GET("/bios", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/dashboard")
	})

	// API docs — custom templ-rendered view of the embedded OpenAPI
	// spec. The raw spec stays public at /redfish/v1/openapi.{yaml,json}
	// for tooling discovery; the rendered docs page is behind auth so
	// it shares the dashboard chrome.
	protected.GET("/docs", apiDocsHandler())
}

// homeModel builds the dashboard's server-rendered state, so the page paints
// in its final shape rather than swapping tabs after load.
func homeModel() pages.HomeModel {
	cfg := config.GetInstance()
	return pages.HomeModel{
		HDMIPrimary:    cfg.Console.HDMIPrimary(),
		ICEServersJSON: iceServersJSON(cfg),
	}
}

// iceServer is the browser's RTCIceServer shape.
//
// Spelled out here rather than reusing webrtc.ICEServer because that type is
// modelled on pion's needs and its JSON encoding is not the dictionary
// RTCPeerConnection expects -- notably it carries extra fields and encodes
// credential as an interface. This is the wire format the browser reads, so it
// is defined by the browser.
type iceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// iceServersJSON renders the configured STUN/TURN servers for the browser.
//
// It mirrors iceServers() in cmd/server, which supplies the same set to the
// BMC's own peer connection; both sides gather against the same servers.
// Returns "[]" and not "" when nothing is configured -- that is the valid
// empty value for RTCPeerConnection, and the common case on a LAN where host
// candidates are sufficient.
func iceServersJSON(cfg *config.Config) string {
	servers := []iceServer{}
	if cfg.Stun != "" {
		servers = append(servers, iceServer{URLs: []string{"stun:" + cfg.Stun}})
	}
	if cfg.Turn.TurnAddr != "" {
		servers = append(servers, iceServer{
			URLs:       []string{"turn:" + cfg.Turn.TurnAddr},
			Username:   cfg.Turn.TurnUser,
			Credential: cfg.Turn.TurnCred,
		})
	}
	b, err := json.Marshal(servers)
	if err != nil {
		slog.Error("ui: marshal ice servers", slog.Any("err", err))
		return "[]"
	}
	return string(b)
}

// apiDocsHandler parses the OpenAPI spec once (sync.Once via the model
// cache in apidocs.go) and renders the pages.APIDocsPage on every request.
func apiDocsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		model, err := loadAPIDocsModel()
		if err != nil {
			slog.ErrorContext(c.Request.Context(), "api docs: load model", slog.Any("err", err))
			c.String(http.StatusInternalServerError, "API docs unavailable: %v", err)
			return
		}
		c.Render(http.StatusOK, render.New(c.Request.Context(), http.StatusOK, pages.APIDocsPage(model)))
	}
}
