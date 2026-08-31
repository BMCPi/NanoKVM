package telemetry

// resources.go exports the BMC's own resource usage on /metrics.
//
// The readings come from pkg/sysinfo, whose sampler runs whether or not
// telemetry is enabled — the drawer's graphs need them either way. What is
// conditional is the export: these instruments are only created when telemetry
// is initialised, so a BMC with telemetry off pays nothing for them and one
// with it on gets cpu/memory/disk on the same endpoint as everything else.
//
// Observable gauges rather than the synchronous Int64Gauge the rest of this
// package uses. A synchronous gauge would need something to push into it on a
// schedule, which is a second sampler racing the first; an observable one is
// pulled at collection time and reports whatever the last sample recorded.

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/pi-bmc/nanokvm-app/pkg/sysinfo"
)

// initResourceMetrics registers the observable gauges. Called from initMetrics.
func initResourceMetrics() {
	m := otel.Meter("github.com/pi-bmc/nanokvm-app")

	var err error
	gauge := func(name, desc, unit string) metric.Float64ObservableGauge {
		g, e := m.Float64ObservableGauge(name,
			metric.WithDescription(desc), metric.WithUnit(unit))
		if e != nil {
			err = e
		}
		return g
	}

	// No unit in the name: the exporter is built without otelprom.WithoutUnits
	// (see telemetry.go), so it appends one from WithUnit — "%" becomes
	// _percent and "By" becomes _bytes. Spelling the unit here too would ship
	// nanokvm_bmc_cpu_utilization_ratio_percent, which names two different
	// units in one metric.
	cpu := gauge("nanokvm_bmc_cpu_utilization",
		"BMC processor busy percentage over the last sampling interval", "%")
	mem := gauge("nanokvm_bmc_memory_utilization",
		"BMC memory in use as a percentage of total, counting reclaimable memory as available", "%")
	disk := gauge("nanokvm_bmc_disk_utilization",
		"Writable data volume in use as a percentage of the space available to it", "%")

	// The absolute figures as well as the ratios. Without the totals a scraper
	// cannot turn a percentage back into headroom, and MemTotal is not a
	// constant it can hardcode — the device tree carves a reserved region out
	// of RAM, so it differs between hardware revisions.
	memUsed := gauge("nanokvm_bmc_memory_used", "BMC memory in use", "By")
	memTotal := gauge("nanokvm_bmc_memory_total", "BMC memory installed and visible to the kernel", "By")
	diskUsed := gauge("nanokvm_bmc_disk_used", "Writable data volume in use", "By")
	diskTotal := gauge("nanokvm_bmc_disk_total", "Writable data volume capacity", "By")

	// Not a ratio and not derivable from one. A BMC stalled on its SD card is
	// idle by every jiffie measure, so cpu_utilization goes *down* at the
	// moment something is wrong; this is the counter that says why. Unitless
	// depth, so no WithUnit — a suffix would misdescribe it.
	blocked, e := m.Int64ObservableGauge("nanokvm_bmc_procs_blocked",
		metric.WithDescription("Tasks in uninterruptible sleep, from /proc/stat procs_blocked"))
	if e != nil {
		err = e
	}

	if err != nil {
		pkgLog.Warn("telemetry: resource instrument creation", slog.Any("err", err))
		return
	}

	// One callback for all eight: sysinfo.LatestUsage takes a mutex, and eight
	// separate callbacks would take it eight times per scrape for the same
	// struct. The Valid flags gate each observation, so a subsystem that could
	// not be read reports nothing rather than a zero an alert would fire on.
	const bytesPerMB = 1024 * 1024
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		u := sysinfo.LatestUsage()
		if u.CPUValid {
			o.ObserveFloat64(cpu, u.CPUPercent)
		}
		if u.MemValid {
			o.ObserveFloat64(mem, u.MemPercent)
			o.ObserveFloat64(memUsed, float64(u.MemUsedMB)*bytesPerMB)
			o.ObserveFloat64(memTotal, float64(u.MemTotalMB)*bytesPerMB)
		}
		if u.DiskValid {
			o.ObserveFloat64(disk, u.DiskPercent)
			o.ObserveFloat64(diskUsed, float64(u.DiskUsedMB)*bytesPerMB)
			o.ObserveFloat64(diskTotal, float64(u.DiskTotalMB)*bytesPerMB)
		}
		if u.ProcsBlockedValid {
			o.ObserveInt64(blocked, int64(u.ProcsBlocked)) //nolint:gosec // a task count, bounded by pid_max
		}
		return nil
	}, cpu, mem, disk, memUsed, memTotal, diskUsed, diskTotal, blocked)
	if err != nil {
		pkgLog.Warn("telemetry: resource callback registration", slog.Any("err", err))
	}
}
