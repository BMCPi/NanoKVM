package telemetry

import (
	"github.com/pi-bmc/nanokvm-app/pkg/config"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// Routes wires up the otelgin middleware (when enabled) and the Prometheus
// scrape endpoint. Must run before any handlers are registered so the
// middleware wraps them all. Gated on Enabled() — set only by a successful
// Init() — so a failed telemetry setup doesn't install middleware over
// no-op providers.
func Routes(r *gin.Engine) {
	if !Enabled() {
		return
	}
	tcfg := config.GetInstance().Telemetry

	r.Use(otelgin.Middleware(tcfg.ServiceName))

	if tcfg.Prometheus.Enabled {
		path := tcfg.Prometheus.Path
		if path == "" {
			path = "/metrics"
		}
		handler := promhttp.HandlerFor(PromRegistry, promhttp.HandlerOpts{
			Registry: PromRegistry,
		})
		r.GET(path, gin.WrapH(handler))
		log.Infof("telemetry: prometheus exposed at %s", path)
	}
}
