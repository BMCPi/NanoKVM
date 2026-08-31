package fragments

// fragments_metrics.go serves the app-metrics panel. It is a read-only
// fragment: the panel polls it, and every render reads the live values out of
// the Prometheus registry that /metrics is backed by, so the page and a scrape
// can never disagree.

import (
	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func metricsFragmentRoutes(g *gin.RouterGroup, _ *deps.Deps) {
	g.GET("/metrics", func(c *gin.Context) {
		renderFragment(c, components.MetricsPanel(telemetry.Gather(), telemetry.History()))
	})
}
