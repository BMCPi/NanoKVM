package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// newTestHistory builds a history ring from readings, oldest first — the
// shape history.samples now needs (see history.go), in place of the raw
// slice literal these tests used before.
func newTestHistory(readings ...counterReading) utils.Ring[counterReading] {
	r := utils.NewRing[counterReading](historyDepth)
	for _, c := range readings {
		r.Append(c)
	}
	return r
}

// TestSnapshotReadsRealExporterNames is the point of the stem matching: the
// OpenTelemetry Prometheus exporter decorates instrument names with unit and
// type suffixes, and those rules move between releases. Rather than assert a
// hardcoded name, this drives the real exporter and checks the values come back
// — so an upgrade that renames a series fails here instead of quietly rendering
// an empty dashboard.
func TestSnapshotReadsRealExporterNames(t *testing.T) {
	conf := config.GetInstance()
	saved := conf.Telemetry
	conf.Telemetry = config.Telemetry{
		Enabled:     true,
		ServiceName: "nanokvm-test",
		Prometheus:  config.Prometheus{Enabled: true, Path: "/metrics"},
	}
	t.Cleanup(func() {
		Shutdown(context.Background())
		conf.Telemetry = saved
	})

	if err := Init(context.Background(), slog.New(slog.DiscardHandler)); err != nil {
		t.Skipf("telemetry init unavailable in this environment: %v", err)
	}
	if !Enabled() {
		t.Skip("telemetry did not enable")
	}

	ctx := context.Background()
	IPMIPacketReceived(ctx)
	IPMIPacketReceived(ctx)
	IPMIPacketSent(ctx)
	IPMISessionOpened(ctx)
	IPMIAuthFailure(ctx, "bad-password")
	PowerOperation(ctx, "on", nil)
	PowerState(ctx, true)

	snap := Gather()
	if !snap.Collected {
		t.Fatal("snapshot reports nothing collected with telemetry enabled")
	}

	// The counters must be found under whatever suffix the exporter chose, and
	// carry the values just recorded.
	packets := map[string]float64{}
	for _, s := range snap.IPMIPackets {
		packets[s.Label] = s.Value
	}
	if got := packets["Received"]; got != 2 {
		t.Errorf("IPMI received = %v, want 2 (exporter may have renamed the series)", got)
	}
	if got := packets["Sent"]; got != 1 {
		t.Errorf("IPMI sent = %v, want 1", got)
	}

	if snap.IPMISessions != 1 {
		t.Errorf("IPMI sessions = %d, want 1", snap.IPMISessions)
	}
	if !snap.PowerOn {
		t.Error("power state should read on")
	}

	// Labelled counters must split by their label rather than collapsing.
	if len(snap.AuthFailures) == 0 {
		t.Error("auth failures did not come back")
	} else if snap.AuthFailures[0].Label != "bad-password" {
		t.Errorf("auth failure label = %q, want bad-password", snap.AuthFailures[0].Label)
	}
	if len(snap.PowerOperations) == 0 {
		t.Error("power operations did not come back")
	}
}

// TestSnapshotWhenTelemetryDisabled covers the default configuration: nothing
// is collected, and the panel is told so rather than being handed zeros it
// would draw as real measurements.
func TestSnapshotWhenTelemetryDisabled(t *testing.T) {
	if Enabled() {
		t.Skip("telemetry is enabled in this process")
	}
	if snap := Gather(); snap.Collected {
		t.Error("snapshot claims to have collected with telemetry disabled")
	}
}

// TestNonZeroDropsEmptyBars pins the rule that an un-incremented counter is an
// absence of measurement, not a zero worth charting.
func TestNonZeroDropsEmptyBars(t *testing.T) {
	got := nonZero([]Sample{{Label: "a", Value: 0}, {Label: "b", Value: 3}, {Label: "c", Value: 0}})
	if len(got) != 1 || got[0].Label != "b" {
		t.Errorf("nonZero = %+v, want only b", got)
	}
}

// TestHistoryChartsIntervalsNotTotals is the point of the history layer: the
// counters are cumulative, so the charts must plot what happened in each
// interval. Plotting the totals would draw a line that only climbs.
func TestHistoryChartsIntervalsNotTotals(t *testing.T) {
	history.mu.Lock()
	saved := history.samples
	history.samples = newTestHistory(
		counterReading{At: time.Now().Add(-30 * time.Second), SerialBytes: 1000, IPMIPackets: 10},
		counterReading{At: time.Now().Add(-15 * time.Second), SerialBytes: 1500, IPMIPackets: 14},
		counterReading{At: time.Now(), SerialBytes: 4000, IPMIPackets: 14},
	)
	history.mu.Unlock()
	t.Cleanup(func() {
		history.mu.Lock()
		history.samples = saved
		history.mu.Unlock()
	})

	points := History()
	if len(points) != 2 {
		t.Fatalf("got %d intervals from 3 samples, want 2", len(points))
	}
	if points[0].SerialBytes != 500 || points[1].SerialBytes != 2500 {
		t.Errorf("serial deltas = %v, %v; want 500, 2500 (totals were charted, not intervals)",
			points[0].SerialBytes, points[1].SerialBytes)
	}
	if points[1].IPMIPackets != 0 {
		t.Errorf("an unchanged counter should chart as 0, got %v", points[1].IPMIPackets)
	}
	if !HasActivity(points) {
		t.Error("HasActivity should be true when an interval moved")
	}
}

// TestHistoryHandlesCounterReset covers a restart: the counter goes backwards,
// which is not a negative amount of traffic.
func TestHistoryHandlesCounterReset(t *testing.T) {
	history.mu.Lock()
	saved := history.samples
	history.samples = newTestHistory(
		counterReading{At: time.Now().Add(-15 * time.Second), SerialBytes: 9000},
		counterReading{At: time.Now(), SerialBytes: 40},
	)
	history.mu.Unlock()
	t.Cleanup(func() {
		history.mu.Lock()
		history.samples = saved
		history.mu.Unlock()
	})

	points := History()
	if len(points) != 1 {
		t.Fatalf("got %d intervals, want 1", len(points))
	}
	if points[0].SerialBytes != 0 {
		t.Errorf("a counter reset charted as %v, want 0", points[0].SerialBytes)
	}
}

// TestHistoryNeedsTwoSamples: one reading is a total, and totals are what the
// bar charts already show.
func TestHistoryNeedsTwoSamples(t *testing.T) {
	history.mu.Lock()
	saved := history.samples
	history.samples = newTestHistory(counterReading{At: time.Now(), SerialBytes: 5})
	history.mu.Unlock()
	t.Cleanup(func() {
		history.mu.Lock()
		history.samples = saved
		history.mu.Unlock()
	})

	if points := History(); points != nil {
		t.Errorf("History() = %v from a single sample, want nil", points)
	}
}
