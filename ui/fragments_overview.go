package ui

// fragments_overview.go serves the Server Overview sidebar. Read fragments
// re-render a card body from live state; mutating fragments answer with a
// toast plus the fw-changed event, which every overview card listens for
// (hx-trigger="… fw-changed from:body"), so a write anywhere refreshes
// every card that shows its consequences.

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/smbios"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func overviewFragmentRoutes(g *gin.RouterGroup) {
	o := g.Group("/overview")

	o.GET("/server", func(c *gin.Context) {
		renderFragment(c, components.OverviewServerBody(overviewServerModel()))
	})
	o.GET("/app-update", func(c *gin.Context) {
		renderFragment(c, components.OverviewAppUpdateBody(overviewAppUpdateModel()))
	})
	o.POST("/app/update", postOverviewAppUpdate)

	o.GET("/bios", func(c *gin.Context) {
		renderFragment(c, components.OverviewBiosBody(overviewBiosModel()))
	})
	o.POST("/bios/update", postOverviewBiosUpdate)
	o.POST("/fw/download", postOverviewFwDownload)
	o.GET("/fw/progress", getOverviewFwProgress)

	o.GET("/kernels", func(c *gin.Context) {
		renderFragment(c, components.OverviewKernelsBody(overviewKernelsModel(c.Query("kernel"))))
	})
	o.POST("/kernel/download", postOverviewKernelDownload)
	o.POST("/kernel/activate", postOverviewKernelActivate)

	o.POST("/boot-override", postOverviewBootOverride)
}

// oemString reads one string out of the Oem.NanoKVM block.
func oemString(sys redfish.ComputerSystem, key string) string {
	block, _ := sys.Oem["NanoKVM"].(map[string]any)
	s, _ := block[key].(string)
	return s
}

// overviewServerModel maps the merged Redfish inventory onto the Server
// Information card.
func overviewServerModel() components.OverviewServer {
	sys := redfish.SystemInventory()

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
		// Module type from SMBIOS, e.g. "8 GiB LPDDR4" — same enrichment the
		// old loadDeviceDetail did from the Memory collection.
		if info, err := smbios.GetStore().Load(); err == nil && info != nil && len(info.Memory) > 0 {
			if t := strings.TrimSuffix(info.Memory[0].Type, "_SDRAM"); t != "" {
				m.Memory += " " + t
			}
		}
	}

	// The onboard MAC lives in the firmware inventory (U-Boot's ethaddr).
	if inv, err := firmware.GetController().GetInventory(); err == nil {
		m.MAC = inv["ethaddr"]
	}

	if oemString(sys, "InventorySource") == "SMBIOS" {
		m.InventorySource = "SMBIOS tables"
	} else {
		m.InventorySource = "U-Boot machine.env"
	}
	return m
}

// overviewAppUpdateModel runs the GitHub release check. Latest is "" when
// upstream is unreachable (closed networks), which renders as no chrome.
func overviewAppUpdateModel() components.OverviewUpdateCheck {
	cur := strings.TrimPrefix(application.CurrentVersion(), "v")
	latest := strings.TrimPrefix(application.LatestVersion(), "v")
	return components.OverviewUpdateCheck{
		Current:         cur,
		Latest:          latest,
		UpdateAvailable: cur != "" && latest != "" && cur != latest,
		Checked:         latest != "",
	}
}

// overviewBiosModel extends the first-paint model with the boot rows from
// the Redfish inventory and the U-Boot release check.
func overviewBiosModel() components.OverviewBios {
	m := components.OverviewBiosFirstPaint()
	sys := redfish.SystemInventory()

	m.DeviceTree = oemString(sys, "DeviceTree")
	m.BootMethods = oemString(sys, "BootMethods")

	target := string(sys.Boot.BootSourceOverrideTarget)
	enabled := string(sys.Boot.BootSourceOverrideEnabled)
	overridden := target != "" && target != "None" && enabled != "" && enabled != "Disabled"
	switch {
	case len(sys.Boot.BootOrder) > 0:
		m.BootTargets = strings.Join(sys.Boot.BootOrder, " → ")
	case overridden:
		m.BootTargets = target
	default:
		m.BootTargets = "default"
	}
	if overridden {
		m.BootOverride = strings.ToLower(enabled) + ": " + target
	} else {
		m.BootOverride = "none"
	}

	info, err := firmware.GetController().GetUBootVersionInfo()
	current := info.Current
	if current == "" {
		current = sys.BiosVersion
	}
	m.UBoot = components.OverviewUpdateCheck{
		Current:         current,
		Latest:          info.Latest,
		UpdateAvailable: info.UpdateAvailable,
		Checked:         err == nil && info.Latest != "",
	}
	return m
}

// overviewKernelsModel resolves the active version with the machine.env
// fallback the JSON API uses: the activation-tracking file wins, machine.env
// covers installs that predate it.
func overviewKernelsModel(selected string) components.OverviewKernels {
	ctrl := firmware.GetController()
	active := ctrl.ActiveUBootVersion()
	if active == "" {
		if info, err := ctrl.GetUBootVersionInfo(); err == nil {
			active = info.Current
		}
	}
	if _, ok := firmware.KernelUBootMap[selected]; !ok {
		selected = ""
	}
	return components.OverviewKernelsModel(selected, active)
}

// ovFwProgressDone is the terminal polling response: swap the poller out,
// toast, and refresh every overview card.
func ovFwProgressDone(c *gin.Context, title string) {
	hxToast(c, "success", title, "")
	appendTrigger(c, map[string]any{"fw-changed": nil})
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<div></div>"))
}

func getOverviewFwProgress(c *gin.Context) {
	if firmware.GetController().IsDownloading() {
		renderFragment(c, components.OvFwPoller())
		return
	}
	ovFwProgressDone(c, "Firmware download complete")
}

func postOverviewAppUpdate(c *gin.Context) {
	if err := application.RunUpdate(); err != nil {
		log.Errorf("ui: application update failed: %s", err)
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

func postOverviewBiosUpdate(c *gin.Context) {
	ctrl := firmware.GetController()
	if ctrl.IsDownloading() {
		hxToast(c, "warning", "Download already in progress", "")
		c.Status(http.StatusConflict)
		return
	}

	// DELIBERATELY DETACHED: the download runs past the request.
	go func() {
		if err := ctrl.UpdateUBoot(); err != nil {
			log.Errorf("ui: u-boot update failed: %v", err)
		}
	}()

	hxToast(c, "info", "U-Boot update started", "Env files are preserved.")
	renderFragment(c, components.OvFwPoller())
}

func postOverviewFwDownload(c *gin.Context) {
	ctrl := firmware.GetController()
	if ctrl.IsDownloading() {
		hxToast(c, "warning", "Download already in progress", "")
		c.Status(http.StatusConflict)
		return
	}

	// DELIBERATELY DETACHED: the download runs past the request.
	go func() {
		if err := ctrl.DownloadAndInit(); err != nil {
			log.Errorf("ui: firmware download failed: %v", err)
		}
	}()

	hxToast(c, "info", "Firmware download started", "")
	renderFragment(c, components.OvFwPoller())
}

func postOverviewKernelDownload(c *gin.Context) {
	kernel := c.PostForm("kernel")
	force := c.PostForm("force") == "true"

	reactivating, err := firmware.GetController().StartKernelDownload(kernel, force)
	if err != nil {
		hxToast(c, "error", "Download not started", err.Error())
		c.Status(http.StatusConflict)
		return
	}

	if reactivating {
		hxToast(c, "info", "Re-downloading "+kernel, "The active image is replaced when the download completes.")
	} else {
		hxToast(c, "info", "Downloading U-Boot for Linux "+kernel, "")
	}
	renderFragment(c, components.OvFwPoller())
}

func postOverviewKernelActivate(c *gin.Context) {
	kernel := c.PostForm("kernel")
	ubootVer, ok := firmware.KernelUBootMap[kernel]
	if !ok {
		hxToast(c, "error", "Activation failed", fmt.Sprintf("unknown kernel version %q", kernel))
		c.Status(http.StatusBadRequest)
		return
	}
	if err := firmware.GetController().ActivateVersionedImage(ubootVer); err != nil {
		hxToast(c, "error", "Activation failed", err.Error())
		c.Status(http.StatusConflict)
		return
	}

	hxToast(c, "success", "U-Boot for Linux "+kernel+" activated", "Env files preserved.")
	appendTrigger(c, map[string]any{"fw-changed": nil})
	c.Status(http.StatusOK)
}

// postOverviewBootOverride serves both boot-override forms (the overview
// card and the power menu): mode selects once/continuous, and "clear" or a
// None target clears the override.
func postOverviewBootOverride(c *gin.Context) {
	target := c.PostForm("boot-override")
	mode := c.PostForm("mode")

	enabled := schemas.OnceBootSourceOverrideEnabled
	switch mode {
	case "continuous":
		enabled = schemas.ContinuousBootSourceOverrideEnabled
	case "clear":
		target, enabled = "None", schemas.DisabledBootSourceOverrideEnabled
	}

	if err := redfish.ApplyBootOverride(schemas.BootSource(target), enabled); err != nil {
		hxToast(c, "error", "Boot override failed", err.Error())
		c.Status(http.StatusBadRequest)
		return
	}

	if target == "" || target == "None" {
		hxToast(c, "success", "Boot override cleared", "")
	} else {
		hxToast(c, "success", "Boot override set", target+" ("+mode+")")
	}
	appendTrigger(c, map[string]any{"fw-changed": nil})
	c.Status(http.StatusOK)
}
