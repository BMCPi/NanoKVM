// Package telemetry sets up OpenTelemetry tracing + metrics and a
// Prometheus scrape endpoint. It is opt-in via config.Telemetry.Enabled.
//
// When enabled it provides:
//   - Auto-instrumentation of Gin HTTP handlers via the otelgin middleware
//     (request count, latency histogram, spans per request).
//   - A Prometheus exporter served on the existing HTTP server at the
//     configured path (default /metrics). It exposes both OTel-collected
//     metrics and any direct prometheus/client_golang registrations.
//   - Optional OTLP gRPC export of traces and metrics when
//     Telemetry.OTLP.Endpoint is set.
//   - Custom application metrics for the IPMI / firmware / power / serial
//     subsystems (see metrics.go).
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// PromRegistry is the registry that backs the /metrics endpoint. It is the
// default global registry so any prometheus.MustRegister() in the codebase
// is picked up automatically. Exposed so the HTTP router can attach the
// promhttp.HandlerFor() handler.
var PromRegistry = defaultPromRegistry()

// defaultPromRegistry asserts prometheus.DefaultRegisterer down to the
// concrete *Registry that promhttp.HandlerFor needs. client_golang sets
// DefaultRegisterer to a *Registry at package-var init time and nothing in
// this codebase ever reassigns it, so the assertion cannot fail in practice
// -- but a checked assertion with a clear panic message is still cheaper and
// more debuggable than an unchecked one, so it stays checked rather than
// suppressed.
func defaultPromRegistry() *prometheus.Registry {
	reg, ok := prometheus.DefaultRegisterer.(*prometheus.Registry)
	if !ok {
		panic("telemetry: prometheus.DefaultRegisterer is not a *prometheus.Registry")
	}
	return reg
}

// Version is the service version reported as a resource attribute. Set by
// main at startup so we don't pull a circular dependency on cmd/server.
var Version = "dev"

var (
	initOnce       sync.Once
	enabled        bool
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider

	// pkgLog is the "telemetry" component logger, set by Init before the
	// once-guarded body below runs. Every other logging entry point in this
	// package (StartSampler, Middleware, Routes, Gather, Shutdown) only
	// touches it from behind Enabled() or a provider pointer that becomes
	// non-nil solely inside a successful Init, so nothing can reach pkgLog
	// before Init sets it — a plain package var is safe here without
	// logger.Holder.
	pkgLog *slog.Logger
)

// Enabled reports whether telemetry was initialized successfully.
func Enabled() bool { return enabled }

// Init configures the global OTel tracer + meter providers and the Prometheus
// exporter. Safe to call once; subsequent calls are no-ops. When telemetry
// is disabled in config this leaves the no-op global providers in place.
func Init(ctx context.Context, log *slog.Logger) error {
	pkgLog = logger.Or(log)

	var initErr error
	initOnce.Do(func() {
		cfg := config.GetInstance().Telemetry
		if !cfg.Enabled {
			return
		}

		res, err := buildResource(ctx, cfg.ServiceName)
		if err != nil {
			initErr = fmt.Errorf("build resource: %w", err)
			return
		}

		// ── Metrics: Prometheus exporter + optional OTLP push ────────────
		readers := []sdkmetric.Reader{}

		if cfg.Prometheus.Enabled {
			// client_golang's default registry auto-registers a process
			// collector with ReportErrors:false, so on some platforms a
			// failing /proc read makes it silently emit nothing — no
			// process_open_fds / _max_fds / _resident_memory_bytes. Swap it
			// for one that surfaces errors, so FD/RSS are observable (and any
			// read failure shows up as a scrape error instead of a blank).
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})
			PromRegistry.Unregister(
				collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			)
			if err := PromRegistry.Register(collectors.NewProcessCollector(
				collectors.ProcessCollectorOpts{ReportErrors: true},
			)); err != nil {
				pkgLog.WarnContext(ctx, "telemetry: process collector", slog.Any("err", err))
			}

			// Bridges OTel metric instruments → the Prometheus registry,
			// so /metrics shows them alongside any native prom collectors.
			promExporter, err := otelprom.New(
				otelprom.WithRegisterer(PromRegistry),
			)
			if err != nil {
				initErr = fmt.Errorf("prometheus exporter: %w", err)
				return
			}
			readers = append(readers, promExporter)
		}

		if cfg.OTLP.Endpoint != "" {
			opts := []otlpmetricgrpc.Option{
				otlpmetricgrpc.WithEndpoint(cfg.OTLP.Endpoint),
			}
			if cfg.OTLP.Insecure {
				opts = append(opts, otlpmetricgrpc.WithInsecure())
			}
			mExp, err := otlpmetricgrpc.New(ctx, opts...)
			if err != nil {
				initErr = fmt.Errorf("otlp metric exporter: %w", err)
				return
			}
			readers = append(readers, sdkmetric.NewPeriodicReader(
				mExp,
				sdkmetric.WithInterval(15*time.Second),
			))
		}

		mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
		for _, r := range readers {
			mpOpts = append(mpOpts, sdkmetric.WithReader(r))
		}
		meterProvider = sdkmetric.NewMeterProvider(mpOpts...)
		otel.SetMeterProvider(meterProvider)

		// ── Traces: optional OTLP gRPC exporter ──────────────────────────
		tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
		if cfg.OTLP.Endpoint != "" {
			topts := []otlptracegrpc.Option{
				otlptracegrpc.WithEndpoint(cfg.OTLP.Endpoint),
			}
			if cfg.OTLP.Insecure {
				topts = append(topts, otlptracegrpc.WithInsecure())
			}
			tExp, err := otlptrace.New(ctx, otlptracegrpc.NewClient(topts...))
			if err != nil {
				initErr = fmt.Errorf("otlp trace exporter: %w", err)
				return
			}
			tpOpts = append(tpOpts, sdktrace.WithBatcher(tExp))
		}
		tracerProvider = sdktrace.NewTracerProvider(tpOpts...)
		otel.SetTracerProvider(tracerProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))

		enabled = true
		initMetrics() // create the application metric instruments
		pkgLog.InfoContext(ctx, "telemetry: enabled",
			slog.String("service", cfg.ServiceName),
			slog.Bool("prometheus", cfg.Prometheus.Enabled),
			slog.String("otlp", cfg.OTLP.Endpoint))
	})
	return initErr
}

// Shutdown flushes any buffered traces / metrics. Safe to call when Init
// failed or was never called.
func Shutdown(ctx context.Context) {
	if tracerProvider != nil {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			pkgLog.WarnContext(ctx, "telemetry: tracer shutdown", slog.Any("err", err))
		}
	}
	if meterProvider != nil {
		if err := meterProvider.Shutdown(ctx); err != nil {
			pkgLog.WarnContext(ctx, "telemetry: meter shutdown", slog.Any("err", err))
		}
	}
}

func buildResource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(Version),
		),
	)
}
