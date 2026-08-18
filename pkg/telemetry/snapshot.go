package telemetry

import (
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
)

// Reading the app's own metrics back out for the dashboard.
//
// The values come from the same Prometheus registry that backs /metrics
// rather than from a second set of counters kept for the UI: one source means
// the panel cannot disagree with what a scrape reports, and it costs nothing
// when nobody is looking.
//
// Names are matched by stem rather than in full. The OpenTelemetry Prometheus
// exporter decorates instrument names with unit and type suffixes (a counter in
// bytes becomes ..._bytes_total), and those rules have changed between exporter
// releases; matching "nanokvm_serial_bytes_received" against whatever suffix is
// current keeps the panel working across an upgrade instead of silently
// rendering zeros.

// Sample is one labelled value — a bar in a chart, or a row in the table view.
type Sample struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// Snapshot is the dashboard's view of the app's metrics at one instant.
// Collected is false when telemetry is switched off, which is the default: the
// instruments are never created then, so there is nothing to read and the panel
// says so rather than drawing a chart full of zeros.
type Snapshot struct {
	Collected bool `json:"collected"`

	// Current state — these are gauges, so they are shown as stat tiles
	// rather than charted: one number each, with no history to plot.
	PowerOn        bool `json:"powerOn"`
	ImagePresented bool `json:"imagePresented"`
	IPMISessions   int  `json:"ipmiSessions"`
	SerialSessions int  `json:"serialSessions"`

	// Cumulative counters, grouped for the charts that compare them. IPMI
	// packets and serial bytes are deliberately separate: they are different
	// units, and putting them on one axis would invite a comparison between
	// a packet count and a byte count that means nothing.
	IPMIPackets       []Sample `json:"ipmiPackets"`
	SerialBytes       []Sample `json:"serialBytes"`
	PowerOperations   []Sample `json:"powerOperations"`
	AuthFailures      []Sample `json:"authFailures"`
	FirmwareDownloads []Sample `json:"firmwareDownloads"`
}

// Gather reads the current metric values. It never fails: a registry that
// cannot be gathered yields an empty snapshot, because a dashboard panel is not
// worth failing a page render over.
func Gather() Snapshot {
	snap := Snapshot{}
	if !Enabled() {
		return snap
	}

	families, err := PromRegistry.Gather()
	if err != nil {
		// Gather returns partial results alongside an error, so this is worth
		// noting but not worth discarding what it did collect.
		log.Debugf("telemetry: gathering metrics for the dashboard: %s", err)
	}
	if len(families) == 0 {
		return snap
	}

	index := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		index[f.GetName()] = f
	}

	snap.Collected = true
	snap.PowerOn = total(index, "nanokvm_power_state") > 0
	snap.ImagePresented = total(index, "nanokvm_firmware_image_presented") > 0
	snap.IPMISessions = int(total(index, "nanokvm_ipmi_sessions_active"))
	snap.SerialSessions = int(total(index, "nanokvm_serial_sessions_active"))

	// Each direction is its own counter rather than one labelled family, so
	// they are assembled here into the shape a bar chart wants.
	snap.IPMIPackets = nonZero([]Sample{
		{Label: "Received", Value: total(index, "nanokvm_ipmi_packets_received")},
		{Label: "Sent", Value: total(index, "nanokvm_ipmi_packets_sent")},
	})
	snap.SerialBytes = nonZero([]Sample{
		{Label: "Received", Value: total(index, "nanokvm_serial_bytes_received")},
		{Label: "Sent", Value: total(index, "nanokvm_serial_bytes_sent")},
	})

	snap.PowerOperations = byLabel(index, "nanokvm_power_operations", "operation")
	snap.AuthFailures = byLabel(index, "nanokvm_ipmi_auth_failures", "reason")
	snap.FirmwareDownloads = byLabel(index, "nanokvm_firmware_downloads", "outcome")

	return snap
}

// find returns the family whose name begins with stem, tolerating the unit and
// type suffixes the exporter appends.
func find(index map[string]*dto.MetricFamily, stem string) *dto.MetricFamily {
	if f, ok := index[stem]; ok {
		return f
	}
	// Deterministic pick: several families can share a stem (a histogram's
	// _sum and _count), and the panel must not change shape between renders.
	names := make([]string, 0, len(index))
	for name := range index {
		if strings.HasPrefix(name, stem) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return index[names[0]]
}

// total sums every series in a family, ignoring labels.
func total(index map[string]*dto.MetricFamily, stem string) float64 {
	f := find(index, stem)
	if f == nil {
		return 0
	}
	var sum float64
	for _, m := range f.GetMetric() {
		sum += value(m)
	}
	return sum
}

// byLabel splits a family into one sample per value of label, sorted largest
// first so the chart reads as a ranking and the bar order is stable.
func byLabel(index map[string]*dto.MetricFamily, stem, label string) []Sample {
	f := find(index, stem)
	if f == nil {
		return nil
	}

	totals := map[string]float64{}
	for _, m := range f.GetMetric() {
		name := "unlabelled"
		for _, pair := range m.GetLabel() {
			if pair.GetName() == label {
				name = pair.GetValue()
				break
			}
		}
		totals[name] += value(m)
	}

	out := make([]Sample, 0, len(totals))
	for name, v := range totals {
		out = append(out, Sample{Label: name, Value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Label < out[j].Label
	})
	return nonZero(out)
}

// value reads whichever field the metric's type populates.
func value(m *dto.Metric) float64 {
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetUntyped() != nil:
		return m.GetUntyped().GetValue()
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	default:
		return 0
	}
}

// nonZero drops empty bars. A counter that has never been incremented is not a
// measurement of zero, it is an absence of measurements, and a chart of flat
// zero bars invites the reader to compare nothing with nothing.
func nonZero(in []Sample) []Sample {
	out := in[:0]
	for _, s := range in {
		if s.Value > 0 {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
