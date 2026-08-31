package sysinfo

// resource_history.go is the always-on side of resource collection: a sampler
// goroutine, a bounded ring of readings, and the accessors the drawer and the
// metrics exporter read.
//
// Always-on, unlike pkg/telemetry's sampler, which only runs when the operator
// has turned telemetry on. The distinction is deliberate. Telemetry's sampler
// gathers the whole Prometheus registry, which is real work and only earns its
// keep when something is scraping it; this one reads three files and costs
// microseconds, and the graphs it feeds are the first thing an operator looks
// at when the BMC feels slow — which is exactly the moment they will not have
// had the foresight to enable telemetry first.

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// resourceInterval between readings. Ten seconds resolves a stalled
	// upload or a runaway process without waking a single core more than it
	// has to; the CPU figure is the mean over the interval either way.
	resourceInterval = 10 * time.Second

	// resourceDepth is how many readings are kept — 180 × 10s = thirty
	// minutes, a few kilobytes. Long enough to see the shape of what just
	// happened, short enough that nobody has to think about the memory.
	resourceDepth = 180
)

// ResourcePoint is one reading, ready to plot.
type ResourcePoint struct {
	// At is the reading's wall-clock time, formatted for the chart's axis.
	At string `json:"at"`
	// CPU is the busy percentage over the preceding interval. Absent from the
	// first reading, which has no interval behind it.
	CPU float64 `json:"cpu"`
	// Memory and Disk are instantaneous percentages at the reading.
	Memory float64 `json:"memory"`
	Disk   float64 `json:"disk"`
}

var resources = struct {
	mu      sync.Mutex
	sampler ResourceSampler
	points  []ResourcePoint
	latest  Usage
	started bool
}{}

// StartResourceSampler records resource usage until ctx is cancelled. Safe to
// call more than once; only the first call starts a goroutine.
func StartResourceSampler(ctx context.Context, log *slog.Logger) {
	resources.mu.Lock()
	if resources.started {
		resources.mu.Unlock()
		return
	}
	resources.started = true
	resources.mu.Unlock()

	// One reading up front so the drawer has something to show before the
	// first tick, and so the CPU sampler is primed — its first reading never
	// yields a percentage, and paying that cost at boot rather than at the
	// operator's first glance is free.
	recordResourceSample()

	go func() {
		ticker := time.NewTicker(resourceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				recordResourceSample()
			}
		}
	}()

	if log != nil {
		log.DebugContext(ctx, "sysinfo: sampling resource usage",
			slog.Duration("interval", resourceInterval),
			slog.Duration("history", resourceDepth*resourceInterval))
	}
}

func recordResourceSample() {
	// Sampled outside the lock: Sample() opens two files and makes a syscall,
	// and holding the mutex across that would block every reader of Latest()
	// on the filesystem.
	resources.mu.Lock()
	sampler := &resources.sampler
	resources.mu.Unlock()

	u := sampler.Sample()

	appendResourcePoint(u, ResourcePoint{
		At:     time.Now().Format("15:04"),
		CPU:    u.CPUPercent,
		Memory: u.MemPercent,
		Disk:   u.DiskPercent,
	})
}

// appendResourcePoint stores one reading, evicting the oldest once the ring is
// full. Copying a 180-element slice every ten seconds is cheaper than the
// bookkeeping a ring index would add, and matches what pkg/telemetry's history
// does for the same reason.
func appendResourcePoint(u Usage, p ResourcePoint) {
	resources.mu.Lock()
	defer resources.mu.Unlock()

	resources.latest = u
	resources.points = append(resources.points, p)
	if len(resources.points) > resourceDepth {
		resources.points = append(resources.points[:0], resources.points[1:]...)
	}
}

// LatestUsage is the most recent reading, for the numbers beside the graphs
// and for the metrics callbacks. The zero Usage until the first sample lands.
func LatestUsage() Usage {
	resources.mu.Lock()
	defer resources.mu.Unlock()
	return resources.latest
}

// ResourceHistory returns the recorded readings, oldest first.
//
// The first reading is dropped: its CPU figure is always zero because a rate
// needs two samples, and a graph that opens with a trough the machine never
// had is worse than one that starts a tick later.
func ResourceHistory() []ResourcePoint {
	resources.mu.Lock()
	defer resources.mu.Unlock()

	if len(resources.points) < 2 {
		return nil
	}
	return append([]ResourcePoint(nil), resources.points[1:]...)
}
