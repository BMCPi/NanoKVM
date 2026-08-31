package redfish

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/app/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
)

func (h *handlers) GetSystemCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"ComputerSystemCollection", "Computer System Collection", systemsPath,
		Link(systemPath),
	))
}

func (h *handlers) GetSystem(c *gin.Context) {
	// The host firmware polls this resource for its boot override, so it is
	// served with the host-interface conventions: exact application/json
	// content type and an ETag it can round-trip.
	writeHostResource(c, hostView(buildSystemResource(c.Request.Context(), h.d.Power)))
}

func (h *handlers) ResetSystem(c *gin.Context) {
	var req struct {
		ResetType schemas.ResetType `json:"ResetType"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if !resetTypeSupported(req.ResetType) {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid ResetType: "+string(req.ResetType))
		return
	}

	ctrl := h.d.Power
	var err error

	// Detached from the request for the same reason as the /api/vm/gpio
	// handler: Redfish clients (gofish, bmclib, the Dell Terraform provider)
	// set short client timeouts, and a timed-out PowerCycle must not leave the
	// host off. See deps.ActionContext.
	ctx, cancel := h.d.ActionContext(power.ActionTimeout)
	defer cancel()

	switch resetOpFor(req.ResetType) {
	case resetOpOn:
		err = ctrl.PowerOn(ctx)
	case resetOpGracefulOff:
		err = ctrl.PowerOff(ctx)
	case resetOpForceOff:
		err = ctrl.ForceOff(ctx)
	case resetOpCycle:
		err = ctrl.Reset(ctx)
	default:
		// Unreachable while these cases cover supportedResetTypes. Catches
		// a value being added to that list without a case here.
		redfishErrorResponse(c, http.StatusNotImplemented,
			"unhandled ResetType: "+string(req.ResetType))
		return
	}

	if err != nil {
		redfishErrorResponse(c, http.StatusInternalServerError,
			string(req.ResetType)+" failed: "+err.Error())
		return
	}

	h.log.DebugContext(c.Request.Context(), "redfish reset action",
		slog.String("resetType", string(req.ResetType)))
	c.Status(http.StatusNoContent)
}

// systemPatchRequest is the writable ComputerSystem surface. Boot is the
// operator's override staging; the identity/progress fields are host reports
// (pointer-typed so "absent" and "empty" stay distinguishable) and are
// accepted only over the host interface.
type systemPatchRequest struct {
	Boot *struct {
		BootSourceOverrideTarget  schemas.BootSource                `json:"BootSourceOverrideTarget"`
		BootSourceOverrideEnabled schemas.BootSourceOverrideEnabled `json:"BootSourceOverrideEnabled"`
		// Mode is accepted but ignored — the host firmware path is
		// UEFI-only, so there is no toggle to honour. buildSystemResource
		// echoes it back so PATCH responses stay consistent.
		BootSourceOverrideMode schemas.BootSourceOverrideMode `json:"BootSourceOverrideMode"`
	} `json:"Boot"`

	BiosVersion  *string `json:"BiosVersion"`
	Manufacturer *string `json:"Manufacturer"`
	Model        *string `json:"Model"`
	SerialNumber *string `json:"SerialNumber"`
	UUID         *string `json:"UUID"`
	BootProgress *struct {
		LastState *string `json:"LastState"`
	} `json:"BootProgress"`
}

// hasHostReport reports whether the PATCH carries host-owned fields.
func (r *systemPatchRequest) hasHostReport() bool {
	return r.BiosVersion != nil || r.Manufacturer != nil || r.Model != nil ||
		r.SerialNumber != nil || r.UUID != nil ||
		(r.BootProgress != nil && r.BootProgress.LastState != nil)
}

// PatchSystem handles both write directions on the ComputerSystem: an
// operator staging a boot override, and the host firmware reporting its
// identity and boot progress. The full system resource is returned — the
// host reads the staged override out of the PATCH response.
func (h *handlers) PatchSystem(c *gin.Context) {
	var req systemPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.hasHostReport() {
		if !hostWritable(c) {
			return
		}
		updateHostReported(func(hr *HostReport) {
			for dst, src := range map[*string]*string{
				&hr.BiosVersion:  req.BiosVersion,
				&hr.Manufacturer: req.Manufacturer,
				&hr.Model:        req.Model,
				&hr.SerialNumber: req.SerialNumber,
				&hr.UUID:         req.UUID,
			} {
				if src != nil {
					*dst = *src
				}
			}
			if req.BootProgress != nil && req.BootProgress.LastState != nil {
				hr.BootProgress = *req.BootProgress.LastState
			}
		})
	}

	if req.Boot != nil {
		if err := applyBootPatch(req.Boot.BootSourceOverrideTarget,
			req.Boot.BootSourceOverrideEnabled); err != nil {
			redfishErrorResponse(c, http.StatusBadRequest, err.Error())
			return
		}
	}

	writeHostResource(c, hostView(buildSystemResource(c.Request.Context(), h.d.Power)))
}

// applyBootPatch validates and stages a boot-override change with the PATCH
// semantics: empty enabled means Once, and Disabled or a None target clears
// the override.
//
// Logs via pkgLog() rather than a threaded logger: it runs both from
// PatchSystem below (which has h.log) and from the exported
// ApplyBootOverride, which the ui package calls directly with nothing to
// hand it. See pkgLogHolder's doc comment in redfish.go.
func applyBootPatch(target schemas.BootSource, enabled schemas.BootSourceOverrideEnabled) error {
	if enabled == "" {
		enabled = schemas.OnceBootSourceOverrideEnabled // Redfish convention
	}
	if !overrideEnabledSupported(enabled) {
		return fmt.Errorf("invalid BootSourceOverrideEnabled: %s", enabled)
	}

	if enabled == schemas.DisabledBootSourceOverrideEnabled || target == schemas.NoneBootSource {
		clearBootOverride()
		pkgLog().Debug("redfish boot override cleared")
		return nil
	}

	if !bootSourceSupported(target) {
		return fmt.Errorf("invalid BootSourceOverrideTarget: %s", target)
	}

	stageBootOverride(target, enabled)
	pkgLog().Debug("redfish boot override staged",
		slog.String("target", string(target)), slog.String("enabled", string(enabled)))
	return nil
}

// SystemInventory returns the ComputerSystem resource for in-process
// consumers — the ui package's overview fragments render the Server
// Information card from it. Same data GET /redfish/v1/Systems/1 serves.
// The firmware controller is no longer a data source (identity is
// host-reported), but the parameter stays so ui call sites keep compiling.
func SystemInventory(ctx context.Context, _ *firmware.Controller, pw *power.Controller) ComputerSystem {
	return buildSystemResource(ctx, pw)
}

// ApplyBootOverride sets or clears the boot-source override for in-process
// consumers (the ui boot-override fragments), with the same semantics as
// PATCH /redfish/v1/Systems/1. The firmware controller parameter is unused
// but kept so ui call sites keep compiling.
func ApplyBootOverride(target schemas.BootSource, enabled schemas.BootSourceOverrideEnabled, _ *firmware.Controller) error {
	return applyBootPatch(target, enabled)
}

func buildSystemResource(ctx context.Context, pw *power.Controller) ComputerSystem {
	powerState := schemas.OffPowerState
	if on, err := pw.State(ctx); err == nil && on {
		powerState = schemas.OnPowerState
	}

	biosLink := Link(biosPath)
	secureBootLink := Link(secureBootPath)
	memoryLink := Link(memoryPath)
	processorsLink := Link(processorsPath)
	storageLink := Link(storageRootPath)
	nicsLink := Link(ethernetInterfacesPath)

	reported, _ := HostReported()

	sys := ComputerSystem{
		Resource: Resource{
			// Pinned to the version the managed host's Redfish client is
			// built against (RedfishClientPkg/Features/ComputerSystem/v1_5_0,
			// whose HII questions are tagged
			// x-UEFI-redfish-ComputerSystem.v1_5_0). Advertising a newer
			// version does not fail outright — the client rewrites
			// @odata.type to its own under PcdRedfishCompatibleSchemaSupport
			// — but it takes a "Compatible mode" fallback path to do it.
			// Matching exactly keeps the client on its normal path.
			ODataType:    odataTypeComputerSystem,
			ODataID:      systemPath,
			ODataContext: odataContext("ComputerSystem.ComputerSystem"),
			ID:           "1",
			Name:         "Computer System",
		},
		SystemType: schemas.PhysicalSystemType,
		PowerState: powerState,
		Boot:       readBoot(),
		// Identity is whatever the host last reported over the host
		// interface; omit-when-empty keeps a never-booted host from
		// advertising blank strings.
		Manufacturer: reported.Manufacturer,
		Model:        reported.Model,
		SerialNumber: reported.SerialNumber,
		UUID:         reported.UUID,
		BiosVersion:  reported.BiosVersion,
		Bios:         &biosLink,
		SecureBoot:   &secureBootLink,
		// Host-owned sub-collections. Always linked: clients descend and
		// find whatever the host has reported, which may be an empty
		// collection before its first boot.
		Memory:             &memoryLink,
		Processors:         &processorsLink,
		Storage:            &storageLink,
		EthernetInterfaces: &nicsLink,
		Actions: &SystemActions{
			Reset: ResetAction{
				Target:            systemResetPath,
				AllowableResetVal: supportedResetTypes,
			},
		},
		Links: &SystemLinks{
			Chassis:   Links{Link(chassisItemPath)},
			ManagedBy: Links{Link(managerPath)},
		},
	}

	if reported.BootProgress != "" {
		sys.BootProgress = &BootProgress{
			LastState: reported.BootProgress,
			// The host reports progress as it happens, so the report time is
			// the state-change time for all practical purposes.
			LastStateTime: reported.ReportedAt.UTC().Format(time.RFC3339),
		}
	}

	return sys
}
