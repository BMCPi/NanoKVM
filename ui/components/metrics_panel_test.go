package components

import (
	"context"
	"strings"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/platform/telemetry"
)

func renderPanel(t *testing.T, snap telemetry.Snapshot, trend ...telemetry.Point) string {
	t.Helper()

	var sb strings.Builder
	if err := MetricsPanel(snap, trend).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestMetricsPanelRendersCharts covers the populated case end to end: the chart
// container, the category labels and the values all have to reach the markup,
// since the chart is drawn client-side from exactly this DOM.
func TestMetricsPanelRendersCharts(t *testing.T) {
	html := renderPanel(t, telemetry.Snapshot{
		Collected:      true,
		PowerOn:        true,
		IPMISessions:   2,
		SerialSessions: 1,
		IPMIPackets:    []telemetry.Sample{{Label: "Received", Value: 1200}, {Label: "Sent", Value: 800}},
		AuthFailures:   []telemetry.Sample{{Label: "bad-password", Value: 3}},
	})

	for _, want := range []string{
		"chart-ipmi-packets", // the container id, which chart.js binds to
		"Received",
		"bad-password",
		"IPMI Packets",
		"IPMI Auth Failures",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered panel is missing %q", want)
		}
	}

	// The stat tiles are numbers, not charts.
	if !strings.Contains(html, ">2<") {
		t.Error("IPMI session count is not rendered as a stat tile")
	}
}

// TestMetricsPanelOmitsEmptyFamilies is the "absence of measurement is not a
// zero" rule at the render layer: a counter nobody has touched gets no card at
// all, rather than an empty chart that reads as inactivity.
func TestMetricsPanelOmitsEmptyFamilies(t *testing.T) {
	html := renderPanel(t, telemetry.Snapshot{
		Collected:   true,
		IPMIPackets: []telemetry.Sample{{Label: "Received", Value: 5}},
	})

	if !strings.Contains(html, "IPMI Packets") {
		t.Error("the populated family should render")
	}
	for _, absent := range []string{"IPMI Auth Failures", "Power Operations", "Firmware Downloads"} {
		if strings.Contains(html, absent) {
			t.Errorf("%q rendered a card with no samples", absent)
		}
	}
}

// TestMetricsPanelDisabledState pins what the default configuration shows.
// Telemetry is off unless switched on, and the panel has to say that rather
// than drawing zeros.
func TestMetricsPanelDisabledState(t *testing.T) {
	html := renderPanel(t, telemetry.Snapshot{Collected: false})

	if !strings.Contains(html, "Metrics are not being collected") {
		t.Error("the disabled state is not explained")
	}
	if strings.Contains(html, "chart-ipmi-packets") {
		t.Error("a chart was rendered with nothing collected")
	}
}

// TestMetricsCompact keeps the on-bar values short. A BMC up for a week counts
// serial bytes in the millions, and eight digits beside a 150px chart is a
// number nobody reads.
func TestMetricsCompact(t *testing.T) {
	for in, want := range map[float64]string{
		42: "42", 1500: "1.5k", 2_400_000: "2.4M", 3_100_000_000: "3.1G",
	} {
		if got := metricsCompact(in); got != want {
			t.Errorf("metricsCompact(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestMetricsTrendsRenderWhenActive covers the trend section: it appears only
// once an interval has actually recorded something.
func TestMetricsTrendsRenderWhenActive(t *testing.T) {
	active := []telemetry.Point{
		{At: "10:00", SerialBytes: 0, IPMIPackets: 0},
		{At: "10:15", SerialBytes: 4096, IPMIPackets: 12},
	}
	html := renderPanel(t, telemetry.Snapshot{Collected: true}, active...)
	for _, want := range []string{"chart-trend-serial", "chart-trend-ipmi", "Serial Throughput", "10:15"} {
		if !strings.Contains(html, want) {
			t.Errorf("active trend is missing %q", want)
		}
	}
}

// TestMetricsTrendsHiddenWhenIdle is the other half: an hour of nothing is not
// a finding worth two charts of flat zero.
func TestMetricsTrendsHiddenWhenIdle(t *testing.T) {
	idle := []telemetry.Point{{At: "10:00"}, {At: "10:15"}}
	if html := renderPanel(t, telemetry.Snapshot{Collected: true}, idle...); strings.Contains(html, "Serial Throughput") {
		t.Error("trend charts rendered for an idle hour")
	}
	if html := renderPanel(t, telemetry.Snapshot{Collected: true}); strings.Contains(html, "Serial Throughput") {
		t.Error("trend charts rendered with no history at all")
	}
}

func renderOverview(t *testing.T, snap telemetry.Snapshot, trend ...telemetry.Point) string {
	t.Helper()

	var sb strings.Builder
	if err := MetricsOverviewBody(snap, trend).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestMetricsOverviewOmitsBarCharts pins the content difference between the two
// cuts. The drawer is 24rem wide and the bar cards reserve a 92px label gutter,
// so they are dropped rather than squeezed — the tiles and trends are what fit.
func TestMetricsOverviewOmitsBarCharts(t *testing.T) {
	html := renderOverview(t, telemetry.Snapshot{
		Collected:      true,
		IPMISessions:   2,
		SerialSessions: 1,
		IPMIPackets:    []telemetry.Sample{{Label: "Received", Value: 1200}},
		AuthFailures:   []telemetry.Sample{{Label: "bad-password", Value: 3}},
	})

	// The tiles are the point of the card and must survive.
	for _, want := range []string{"IPMI Sessions", "Serial Sessions", "Host Power", ">2<"} {
		if !strings.Contains(html, want) {
			t.Errorf("overview body is missing %q", want)
		}
	}
	// The bar cards belong to the settings panel only.
	for _, absent := range []string{"chart-ipmi-packets", "IPMI Auth Failures", "Cumulative since boot"} {
		if strings.Contains(html, absent) {
			t.Errorf("overview body rendered %q, which belongs to the wide panel", absent)
		}
	}
}

// TestMetricsOverviewChartIDsAreDistinct is the load-bearing one: the settings
// panel and the drawer are both in the DOM at once, and the chart runtime keys
// its instances by container id. Colliding ids would leave one of the two
// plotting into the other's canvas.
func TestMetricsOverviewChartIDsAreDistinct(t *testing.T) {
	active := []telemetry.Point{
		{At: "10:00", SerialBytes: 0, IPMIPackets: 0},
		{At: "10:15", SerialBytes: 4096, IPMIPackets: 12},
	}
	snap := telemetry.Snapshot{Collected: true}

	panel := renderPanel(t, snap, active...)
	drawer := renderOverview(t, snap, active...)

	for _, id := range []string{"chart-ov-trend-serial", "chart-ov-trend-ipmi"} {
		if !strings.Contains(drawer, id) {
			t.Errorf("drawer trend is missing container %q", id)
		}
		if strings.Contains(panel, id) {
			t.Errorf("settings panel also rendered %q; the ids must not collide", id)
		}
	}
	for _, id := range []string{`id="chart-trend-serial"`, `id="chart-trend-ipmi"`} {
		if strings.Contains(drawer, id) {
			t.Errorf("drawer reused the settings panel's container %q", id)
		}
	}
}

// TestMetricsOverviewDisabledState keeps the drawer's empty state terse. The
// long explanation belongs to the settings panel, where it is the whole content;
// here it is one card of four and only needs to point at the switch.
func TestMetricsOverviewDisabledState(t *testing.T) {
	html := renderOverview(t, telemetry.Snapshot{Collected: false})

	if !strings.Contains(html, "Not being collected") {
		t.Error("the drawer does not say why it is empty")
	}
	if strings.Contains(html, "Metrics are not being collected") {
		t.Error("the drawer used the wide panel's full notice")
	}
	if strings.Contains(html, "chart-ov-trend") {
		t.Error("a chart was rendered with nothing collected")
	}
}

// TestMetricsOverviewTrendsHiddenWhenIdle mirrors the panel's rule: a flat hour
// is not worth two charts.
func TestMetricsOverviewTrendsHiddenWhenIdle(t *testing.T) {
	idle := []telemetry.Point{{At: "10:00"}, {At: "10:15"}}
	html := renderOverview(t, telemetry.Snapshot{Collected: true}, idle...)

	if strings.Contains(html, "chart-ov-trend") {
		t.Error("trend charts rendered for an idle hour")
	}
	if !strings.Contains(html, "IPMI Sessions") {
		t.Error("the tiles should still render when the trends are hidden")
	}
}
