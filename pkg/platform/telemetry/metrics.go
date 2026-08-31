package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Application-wide metric instruments. They are created lazily by initMetrics
// and remain no-op (nil-safe via the helper methods below) until telemetry
// is enabled. Service packages should always call the helpers, never touch
// these directly.
var (
	// IPMI
	ipmiPacketsReceived metric.Int64Counter
	ipmiPacketsSent     metric.Int64Counter
	ipmiSessionsActive  metric.Int64UpDownCounter
	ipmiAuthFailures    metric.Int64Counter

	// Firmware
	firmwareDownloadsTotal   metric.Int64Counter
	firmwareDownloadDuration metric.Float64Histogram
	firmwareImagePresented   metric.Int64UpDownCounter

	// Power
	powerOperationsTotal metric.Int64Counter
	powerStateGauge      metric.Int64Gauge

	// Serial
	serialSessionsActive metric.Int64UpDownCounter
	serialBytesRx        metric.Int64Counter
	serialBytesTx        metric.Int64Counter
)

func initMetrics() {
	m := otel.Meter("github.com/pi-bmc/nanokvm-app")

	var err error
	mustCounter := func(name, desc, unit string) metric.Int64Counter {
		c, e := m.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if e != nil {
			err = e
		}
		return c
	}
	mustUpDown := func(name, desc string) metric.Int64UpDownCounter {
		c, e := m.Int64UpDownCounter(name, metric.WithDescription(desc))
		if e != nil {
			err = e
		}
		return c
	}
	mustHist := func(name, desc, unit string) metric.Float64Histogram {
		h, e := m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if e != nil {
			err = e
		}
		return h
	}
	mustGauge := func(name, desc string) metric.Int64Gauge {
		g, e := m.Int64Gauge(name, metric.WithDescription(desc))
		if e != nil {
			err = e
		}
		return g
	}

	ipmiPacketsReceived = mustCounter("nanokvm_ipmi_packets_received_total",
		"IPMI UDP packets received", "{packet}")
	ipmiPacketsSent = mustCounter("nanokvm_ipmi_packets_sent_total",
		"IPMI UDP packets sent", "{packet}")
	ipmiSessionsActive = mustUpDown("nanokvm_ipmi_sessions_active",
		"Currently active IPMI sessions")
	ipmiAuthFailures = mustCounter("nanokvm_ipmi_auth_failures_total",
		"IPMI authentication failures (RAKP/openSession rejects)", "{failure}")

	// Metric names predate FMP capsule delivery and are kept stable for
	// existing scrapers; they now count capsule fetches and the capsule
	// volume's presentation on the gadget.
	firmwareDownloadsTotal = mustCounter("nanokvm_firmware_downloads_total",
		"Firmware capsule download attempts, labelled by outcome", "{download}")
	firmwareDownloadDuration = mustHist("nanokvm_firmware_download_duration_seconds",
		"Time to download a firmware capsule", "s")
	firmwareImagePresented = mustUpDown("nanokvm_firmware_image_presented",
		"1 when the USB gadget is presenting the capsule volume, 0 otherwise")

	powerOperationsTotal = mustCounter("nanokvm_power_operations_total",
		"Power control operations issued, labelled by op and outcome", "{op}")
	powerStateGauge = mustGauge("nanokvm_power_state",
		"Current target power state (1=on, 0=off) as last observed")

	serialSessionsActive = mustUpDown("nanokvm_serial_sessions_active",
		"Currently connected serial console sessions")
	serialBytesRx = mustCounter("nanokvm_serial_bytes_received_total",
		"Bytes read from the serial port", "By")
	serialBytesTx = mustCounter("nanokvm_serial_bytes_sent_total",
		"Bytes written to the serial port", "By")

	if err != nil {
		pkgLog().Warn("telemetry: instrument creation", slog.Any("err", err))
	}

	// The BMC's own cpu/memory/disk, observed from pkg/sysinfo's always-on
	// sampler. See resources.go for why they are observable rather than
	// pushed.
	initResourceMetrics()

	// What the managed host reports about itself, pushed over I2C from OP-TEE.
	// See host_sensors.go.
	initHostSensorMetrics()
}

// ── Helpers used by service packages. All are nil-safe so calls are free
// when telemetry is disabled.

func IPMIPacketReceived(ctx context.Context) {
	if ipmiPacketsReceived != nil {
		ipmiPacketsReceived.Add(ctx, 1)
	}
}

func IPMIPacketSent(ctx context.Context) {
	if ipmiPacketsSent != nil {
		ipmiPacketsSent.Add(ctx, 1)
	}
}

func IPMISessionOpened(ctx context.Context) {
	if ipmiSessionsActive != nil {
		ipmiSessionsActive.Add(ctx, 1)
	}
}

func IPMISessionClosed(ctx context.Context) {
	if ipmiSessionsActive != nil {
		ipmiSessionsActive.Add(ctx, -1)
	}
}

func IPMIAuthFailure(ctx context.Context, reason string) {
	if ipmiAuthFailures != nil {
		ipmiAuthFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

func FirmwareDownload(ctx context.Context, outcome string, seconds float64) {
	if firmwareDownloadsTotal != nil {
		firmwareDownloadsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	if firmwareDownloadDuration != nil && seconds > 0 {
		firmwareDownloadDuration.Record(ctx, seconds, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

func FirmwarePresented(ctx context.Context, presented bool) {
	if firmwareImagePresented == nil {
		return
	}
	if presented {
		firmwareImagePresented.Add(ctx, 1)
	} else {
		firmwareImagePresented.Add(ctx, -1)
	}
}

func PowerOperation(ctx context.Context, op string, err error) {
	if powerOperationsTotal == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	powerOperationsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("op", op),
		attribute.String("outcome", outcome),
	))
}

func PowerState(ctx context.Context, on bool) {
	if powerStateGauge == nil {
		return
	}
	v := int64(0)
	if on {
		v = 1
	}
	powerStateGauge.Record(ctx, v)
}

func SerialSessionOpened(ctx context.Context) {
	if serialSessionsActive != nil {
		serialSessionsActive.Add(ctx, 1)
	}
}

func SerialSessionClosed(ctx context.Context) {
	if serialSessionsActive != nil {
		serialSessionsActive.Add(ctx, -1)
	}
}

func SerialBytesRx(ctx context.Context, n int) {
	if serialBytesRx != nil && n > 0 {
		serialBytesRx.Add(ctx, int64(n))
	}
}

func SerialBytesTx(ctx context.Context, n int) {
	if serialBytesTx != nil && n > 0 {
		serialBytesTx.Add(ctx, int64(n))
	}
}
