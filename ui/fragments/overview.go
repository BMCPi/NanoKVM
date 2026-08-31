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
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func overviewFragmentRoutes(g *gin.RouterGroup, d *deps.Deps) {
	o := g.Group("/overview")

	o.GET("/server", func(c *gin.Context) {
		renderFragment(c, components.OverviewServerBody(overviewServerModel(c.Request.Context(), d)))
	})
	o.GET("/app-update", func(c *gin.Context) {
		renderFragment(c, components.OverviewAppUpdateBody(overviewAppUpdateModel(c.Request.Context())))
	})
	o.POST("/app/update", postOverviewAppUpdate)

	// Bound to the drawer's open event rather than page load; see
	// overviewActivityCard for why. Reads the same registry and history the
	// settings Metrics panel does, so the two can never disagree.
	o.GET("/activity", func(c *gin.Context) {
		renderFragment(c, components.MetricsOverviewBody(telemetry.Gather(), telemetry.History()))
	})

	o.GET("/firmware", func(c *gin.Context) {
		renderFragment(c, components.OverviewHostFirmwareBody(overviewFirmwareModel(c.Request.Context(), d)))
	})

	o.GET("/boot-override", func(c *gin.Context) {
		renderFragment(c, components.OverviewBootOverrideBody(overviewBootOverrideModel(c.Request.Context(), d)))
	})
	o.POST("/boot-override", postOverviewBootOverride(d))
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
func overviewAppUpdateModel(ctx context.Context) components.OverviewUpdateCheck {
	cur := strings.TrimPrefix(application.CurrentVersion(), "v")
	latest := strings.TrimPrefix(application.LatestVersion(ctx), "v")
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

// overviewBootOverrideModel feeds the Boot Override card's staging form.
func overviewBootOverrideModel(ctx context.Context, d *deps.Deps) components.OverviewBootOverride {
	return stagedBootOverride(ctx, d)
}

func postOverviewAppUpdate(c *gin.Context) {
	// The process context, not the request's: an update replaces the running
	// binary and then restarts the process, so a browser navigating away must
	// not abandon it half-installed. See deps.ActionContext.
	ctx, cancel := deps.FromContext(c).ActionContext(appUpdateTimeout)
	defer cancel()

	if err := application.RunUpdate(ctx); err != nil {
		slog.ErrorContext(c.Request.Context(), "ui: application update failed", slog.Any("err", err))
		hxToast(c, "error", "Update failed", err.Error())
		c.Status(http.StatusInternalServerError)
		return
	}

	hxToast(c, "info", "Update installed", "Restarting — the page reloads in a moment.")
	c.Status(http.StatusOK)

	// DELIBERATELY DETACHED: the restart must outlive this request; the
	// response has to flush before the service goes down.
	go func() {
		time.Sleep(1 * time.Second)
		application.RestartService()
	}()
}

// postOverviewBootOverride serves both boot-override forms (the overview
// card and the power menu). The clicked submit button supplies mode:
// "once" / "continuous" stage the selected target with that persistence,
// "clear" disables the override regardless of the select. The override is
// BMC state only — the host firmware reads and applies it at its next boot.
func postOverviewBootOverride(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
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

		if err := redfish.ApplyBootOverride(schemas.BootSource(target), enabled, d.Firmware); err != nil {
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
}
