package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// A short rolling history, so the dashboard can show movement rather than only
// totals.
//
// The counters this reads are cumulative: charting them directly draws a line
// that only ever climbs, whose slope is the interesting part and whose height
// says nothing. So each sample is stored as read and the *differences* between
// consecutive samples are what the charts plot — bytes in the last interval,
// packets in the last interval. That also makes a counter reset (the app
// restarting) visible as a gap rather than as a huge negative spike.
//
// Everything is in memory and bounded. A BMC with 128 MB of RAM does not get a
// time-series database for a settings panel; it gets an hour of coarse samples
// costing a few kilobytes, which is enough to answer "is something happening
// right now" — the question the panel exists for.

const (
	// sampleInterval between reads. Coarse on purpose: the panel answers
	// "recently", and gathering the registry on a 1 GHz single core is not
	// something to do every second for a pane nobody may be looking at.
	sampleInterval = 15 * time.Second
	// historyDepth is how many samples are kept — 240 × 15s = one hour.
	historyDepth = 240
)

// counterReading is one instant's cumulative values, keyed by the series the
// trend charts draw.
type counterReading struct {
	At           time.Time
	IPMIPackets  float64
	SerialBytes  float64
	PowerOps     float64
	AuthFailures float64
}

// Point is one plotted interval: what accumulated between two samples.
type Point struct {
	// At is the end of the interval, formatted for the axis.
	At string `json:"at"`
	// Values are per-series deltas over the interval.
	IPMIPackets  float64 `json:"ipmiPackets"`
	SerialBytes  float64 `json:"serialBytes"`
	PowerOps     float64 `json:"powerOps"`
	AuthFailures float64 `json:"authFailures"`
}

var history = struct {
	mu      sync.Mutex
	samples utils.Ring[counterReading]
	started bool
}{samples: utils.NewRing[counterReading](historyDepth)}

// StartSampler begins recording history until ctx is cancelled. Safe to call
// more than once; only the first call starts a sampler. A no-op when telemetry
// is off, because there is then nothing to sample.
func StartSampler(ctx context.Context) {
	if !Enabled() {
		return
	}

	history.mu.Lock()
	if history.started {
		history.mu.Unlock()
		return
	}
	history.started = true
	history.mu.Unlock()

	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()

		// A first reading immediately, so the panel has a baseline to
		// difference against rather than being empty for the first interval.
		recordSample()

		for {
			select {
			case <-ctx.Done():
				pkgLog().DebugContext(ctx, "telemetry: metrics sampler stopped")
				return
			case <-ticker.C:
				recordSample()
			}
		}
	}()
	pkgLog().DebugContext(ctx, "telemetry: sampling metrics",
		slog.Duration("interval", sampleInterval),
		slog.Duration("history", time.Duration(historyDepth)*sampleInterval))
}

func recordSample() {
	snap := Gather()
	if !snap.Collected {
		return
	}

	reading := counterReading{
		At:           time.Now(),
		IPMIPackets:  sum(snap.IPMIPackets),
		SerialBytes:  sum(snap.SerialBytes),
		PowerOps:     sum(snap.PowerOperations),
		AuthFailures: sum(snap.AuthFailures),
	}

	history.mu.Lock()
	defer history.mu.Unlock()

	history.samples.Append(reading)
}

// History returns the recorded intervals, oldest first. Empty until at least
// two samples exist: one reading is a total, and a total is what the bar charts
// already show.
func History() []Point {
	history.mu.Lock()
	samples := history.samples.Snapshot()
	history.mu.Unlock()

	if len(samples) < 2 {
		return nil
	}

	points := make([]Point, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		prev, cur := samples[i-1], samples[i]
		points = append(points, Point{
			At:           cur.At.Format("15:04"),
			IPMIPackets:  delta(prev.IPMIPackets, cur.IPMIPackets),
			SerialBytes:  delta(prev.SerialBytes, cur.SerialBytes),
			PowerOps:     delta(prev.PowerOps, cur.PowerOps),
			AuthFailures: delta(prev.AuthFailures, cur.AuthFailures),
		})
	}
	return points
}

// HasActivity reports whether any interval recorded anything. A history of
// nothing but zeros is not worth four charts of flat lines.
func HasActivity(points []Point) bool {
	for _, p := range points {
		if p.IPMIPackets > 0 || p.SerialBytes > 0 || p.PowerOps > 0 || p.AuthFailures > 0 {
			return true
		}
	}
	return false
}

// delta is the increase between two readings of a cumulative counter. A
// decrease means the counter was reset — the process restarted — and there is
// no meaningful interval across that boundary, so it reads as zero rather than
// as a negative spike.
func delta(prev, cur float64) float64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func sum(samples []Sample) float64 {
	var total float64
	for _, s := range samples {
		total += s.Value
	}
	return total
}
