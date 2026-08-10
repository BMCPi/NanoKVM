package router

import (
	"net/http"

	"github.com/pi-bmc/nanokvm-app/server/config"
	"github.com/pi-bmc/nanokvm-app/server/middleware"
	"github.com/pi-bmc/nanokvm-app/server/telemetry"
	"github.com/pi-bmc/nanokvm-app/ui"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

func Init(r *gin.Engine) {
	telemetryRoutes(r)
	ui.Register(r)
	authCheck(r)
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

// authCheck is the token validation endpoint the login page (and any other
// client) uses for redirect decisions.
func authCheck(r *gin.Engine) {
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
