package telemetry

// gauge.go is the shared instrument-creation helper initHostSensorMetrics
// and initResourceMetrics both need: a batch of Float64ObservableGauges from
// constant, always-valid arguments, where any creation failure is worth
// reporting once at the end rather than checking after every call.

import "go.opentelemetry.io/otel/metric"

// gaugeFactory accumulates the error from a batch of gauge creations.
type gaugeFactory struct {
	m   metric.Meter
	err error
}

// float64Gauge creates one Float64ObservableGauge, recording any error (the
// last one wins — the callers here only report that *a* gauge failed, not
// which) rather than returning it, so a whole batch can be created inline
// and checked once.
func (f *gaugeFactory) float64Gauge(name, desc, unit string) metric.Float64ObservableGauge {
	g, err := f.m.Float64ObservableGauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		f.err = err
	}
	return g
}
