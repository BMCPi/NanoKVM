package telemetry

import (
	"log/slog"

	"github.com/pi-bmc/nanokvm-app/pkg/config"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// Middleware installs the otelgin tracing middleware. Gated on Enabled() —
// set only by a successful Init() — so a failed telemetry setup doesn't
// install middleware over no-op providers.
//
// This MUST be the first middleware on the engine, before the request logger
// and the recovery handler. otelgin puts the span on c.Request and restores
// the original request when it returns, so anything registered OUTSIDE it
// sees no span at all: a request logger registered first silently logs every
// line without trace_id, and the traces and logs can never be joined. The
// order is pinned by TestMiddlewareOrderYieldsTraceIDs in pkg/middleware.
func Middleware(r *gin.Engine) {
	if !Enabled() {
		return
	}
	r.Use(otelgin.Middleware(config.GetInstance().Telemetry.ServiceName))
}

// Routes registers the Prometheus scrape endpoint. Call it after the engine's
// middleware is installed so the endpoint is covered by it.
func Routes(r *gin.Engine) {
	if !Enabled() {
		return
	}
	tcfg := config.GetInstance().Telemetry

	if tcfg.Prometheus.Enabled {
		path := tcfg.Prometheus.Path
		if path == "" {
			path = "/metrics"
		}
		handler := promhttp.HandlerFor(PromRegistry, promhttp.HandlerOpts{
			Registry: PromRegistry,
		})
		r.GET(path, gin.WrapH(handler))
		slog.Info("telemetry: prometheus exposed", slog.String("path", path))
	}
}
