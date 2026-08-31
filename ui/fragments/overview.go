package fragments

// fragments_overview.go serves the Server Overview sidebar. Read fragments
// re-render a card body from live state; mutating fragments answer with a
// toast plus the fw-changed event, which every overview card listens for
// (hx-trigger="… fw-changed from:body"), so a write anywhere refreshes
// every card that shows its consequences.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/sysinfo"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func overviewFragmentRoutes(g *gin.RouterGroup, h *handlers) {
	o := g.Group("/overview")

	o.GET("/server", func(c *gin.Context) {
		renderFragment(c, components.OverviewServerBody(overviewServerModel(c.Request.Context(), h.d)))
	})
	o.GET("/app-update", func(c *gin.Context) {
		renderFragment(c, components.OverviewAppUpdateBody(overviewAppUpdateModel(c.Request.Context(), h.log)))
	})
	o.POST("/app/update", h.postOverviewAppUpdate)

	// Bound to the drawer's open event rather than page load; see
	// overviewActivityCard for why. Reads the same registry and history the
	// settings Metrics panel does, so the two can never disagree.
	o.GET("/activity", func(c *gin.Context) {
		renderFragment(c, components.MetricsOverviewBody(telemetry.Gather(), telemetry.History()))
	})

	// Also bound to the open event: the readings move, and a page-load render
	// would show whatever was true when the tab was opened.
	o.GET("/resources", func(c *gin.Context) {
		renderFragment(c, components.OverviewResourcesBody(overviewResourcesModel()))
	})

	o.GET("/firmware", func(c *gin.Context) {
		renderFragment(c, components.OverviewHostFirmwareBody(overviewFirmwareModel(c.Request.Context(), h.d)))
	})

	// POST only: the Server Overview no longer carries a staging form, but
	// the power menu's still posts here (see PowerBootOverride in
	// power_menu.templ), so the route outlives the card it was written for.
	o.POST("/boot-override", h.postOverviewBootOverride)
}

// oemString reads one string out of the Oem.NanoKVM block.
func oemString(sys redfish.ComputerSystem, key string) string {
	block, _ := sys.Oem["NanoKVM"].(map[string]any)
	s, _ := block[key].(string)
	return s
}

// overviewServerModel maps the Redfish system inventory onto the Server
// Information card.
func overviewServerModel(ctx context.Context, d *deps.Deps) components.OverviewServer {
	sys := redfish.SystemInventory(ctx, d.Firmware, d.Power)

	m := components.OverviewServer{
		Board:    sys.Model,
		Vendor:   sys.Manufacturer,
		Serial:   sys.SerialNumber,
		Revision: sys.SubModel,
	}

	var cpu []string
	if sys.ProcessorSummary != nil && sys.ProcessorSummary.Model != "" {
		cpu = append(cpu, sys.ProcessorSummary.Model)
	}
	if soc := oemString(sys, "SoC"); soc != "" {
		cpu = append(cpu, soc)
	}
	m.CPU = strings.Join(cpu, " / ")

	if sys.MemorySummary != nil && sys.MemorySummary.TotalSystemMemoryGiB != nil && *sys.MemorySummary.TotalSystemMemoryGiB > 0 {
		m.Memory = fmt.Sprintf("%g GiB", *sys.MemorySummary.TotalSystemMemoryGiB)
	}

	// Everything the BMC knows about the host is what the host reported
	// over the host interface.
	m.InventorySource = "host-reported inventory"
	return m
}

// appUpdateTimeout bounds a self-update: release lookup, download and install.
const appUpdateTimeout = 30 * time.Minute

// overviewAppUpdateModel runs the GitHub release check. Latest is "" when
// upstream is unreachable (closed networks), which renders as no chrome.
func overviewAppUpdateModel(ctx context.Context, log *slog.Logger) components.OverviewUpdateCheck {
	cur := strings.TrimPrefix(application.CurrentVersion(), "v")
	latest := strings.TrimPrefix(application.LatestVersion(ctx, log), "v")
	return components.OverviewUpdateCheck{
		Current:         cur,
		Latest:          latest,
		UpdateAvailable: cur != "" && latest != "" && cur != latest,
		Checked:         latest != "",
	}
}

// stagedBootOverride reads the staged override out of the system inventory's
// Boot block — the same state PATCH /redfish/v1/Systems/1 writes and the
// host firmware reads at boot.
func stagedBootOverride(ctx context.Context, d *deps.Deps) components.OverviewBootOverride {
	sys := redfish.SystemInventory(ctx, d.Firmware, d.Power)
	return components.OverviewBootOverride{
		Target:  string(sys.Boot.BootSourceOverrideTarget),
		Enabled: string(sys.Boot.BootSourceOverrideEnabled),
	}
}

// overviewFirmwareModel builds the Host Firmware card from the system
// inventory: host-reported BIOS version and boot progress, plus the staged
// boot override.
func overviewFirmwareModel(ctx context.Context, d *deps.Deps) components.OverviewHostFirmware {
	sys := redfish.SystemInventory(ctx, d.Firmware, d.Power)
	reported, _ := redfish.HostReported()

	bios := sys.BiosVersion
	if bios == "" {
		bios = reported.BiosVersion
	}
	return components.OverviewHostFirmware{
		BiosVersion:  bios,
		BootOverride: stagedBootOverride(ctx, d).BootOverrideLabel(),
		BootProgress: reported.BootProgress,
	}
}

func (h *handlers) postOverviewAppUpdate(c *gin.Context) {
	// The process context, not the request's: an update replaces the running
	// binary and then restarts the process, so a browser navigating away must
	// not abandon it half-installed. See deps.ActionContext.
	ctx, cancel := h.d.ActionContext(appUpdateTimeout)
	defer cancel()

	if err := application.RunUpdate(ctx, h.log); err != nil {
		h.log.ErrorContext(c.Request.Context(), "ui: application update failed", slog.Any("err", err))
		hxToast(c, "error", "Update failed", err.Error())
		c.Status(http.StatusInternalServerError)
		return
	}

	hxToast(c, "info", "Update installed", "Restarting — the page reloads in a moment.")
	c.Status(http.StatusOK)

	// DELIBERATELY DETACHED: the restart must outlive this request; the
	// response has to flush before the service goes down.
	log := h.log
	go func() {
		time.Sleep(1 * time.Second)
		application.RestartService(log)
	}()
}

// postOverviewBootOverride serves both boot-override forms (the overview
// card and the power menu). The clicked submit button supplies mode:
// "once" / "continuous" stage the selected target with that persistence,
// "clear" disables the override regardless of the select. The override is
// BMC state only — the host firmware reads and applies it at its next boot.
func (h *handlers) postOverviewBootOverride(c *gin.Context) {
	target := c.PostForm("boot-override")
	mode := c.PostForm("mode")

	enabled := schemas.OnceBootSourceOverrideEnabled
	switch mode {
	case "once", "":
		// Once is the Redfish default when persistence is unspecified.
	case "continuous":
		enabled = schemas.ContinuousBootSourceOverrideEnabled
	case "clear":
		target, enabled = "None", schemas.DisabledBootSourceOverrideEnabled
	default:
		hxToast(c, "error", "Boot override failed", fmt.Sprintf("unknown mode %q", mode))
		c.Status(http.StatusBadRequest)
		return
	}

	if err := redfish.ApplyBootOverride(schemas.BootSource(target), enabled, h.d.Firmware); err != nil {
		hxToast(c, "error", "Boot override failed", err.Error())
		c.Status(http.StatusBadRequest)
		return
	}

	if target == "" || target == "None" {
		hxToast(c, "success", "Boot override cleared", "The host boots from its normal boot order next time.")
	} else {
		persistence := "once"
		if enabled == schemas.ContinuousBootSourceOverrideEnabled {
			persistence = "persistent"
		}
		hxToast(c, "success", "Boot override staged",
			target+" ("+persistence+") — the host firmware picks it up at its next boot.")
	}
	appendTrigger(c, map[string]any{"fw-changed": nil})
	c.Status(http.StatusOK)
}

// overviewResourcesModel turns the sampler's history into the three traces the
// Resources card draws.
//
// Sampling stays false until there are at least two readings, because the
// first has no CPU figure behind it (a rate needs an interval) and a card that
// opens with one point is a card showing a single dot.
func overviewResourcesModel() components.OverviewResources {
	history := sysinfo.ResourceHistory()
	if len(history) == 0 {
		return components.OverviewResources{}
	}

	cpu := make([]float64, 0, len(history))
	mem := make([]float64, 0, len(history))
	disk := make([]float64, 0, len(history))
	for _, p := range history {
		cpu = append(cpu, p.CPU)
		mem = append(mem, p.Memory)
		disk = append(disk, p.Disk)
	}

	// The latest reading rather than the last history point: history drops its
	// first entry, and on a freshly booted BMC the two can differ by a tick.
	u := sysinfo.LatestUsage()
	return components.OverviewResources{
		Sampling: true,
		CPU: components.ResourceSeries{
			Label: "Processor", Percent: u.CPUPercent, Points: cpu, Valid: u.CPUValid,
		},
		Memory: components.ResourceSeries{
			Label:   "Memory",
			Detail:  resourceDetail(u.MemUsedMB, u.MemTotalMB),
			Percent: u.MemPercent, Points: mem, Valid: u.MemValid,
		},
		Disk: components.ResourceSeries{
			Label:   "Storage",
			Detail:  resourceDetail(u.DiskUsedMB, u.DiskTotalMB),
			Percent: u.DiskPercent, Points: disk, Valid: u.DiskValid,
		},
	}
}

// resourceDetail is the absolute figure beside a percentage: "161 / 246 MB" on
// this device's memory, "1.2 / 6.8 GB" on its data volume. The unit is chosen
// from the total so both halves are always in the same one — "900 MB / 6.8 GB"
// makes the reader do arithmetic the card exists to save them.
func resourceDetail(usedMB, totalMB uint64) string {
	if totalMB == 0 {
		return ""
	}
	if totalMB < 1024 {
		return fmt.Sprintf("%d / %d MB", usedMB, totalMB)
	}
	const mbPerGB = 1024.0
	return fmt.Sprintf("%.1f / %.1f GB", float64(usedMB)/mbPerGB, float64(totalMB)/mbPerGB)
}
