package redfish

import (
	"net/http"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

func (s *Service) GetSystemCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"ComputerSystemCollection", "Computer System Collection", systemsPath,
		Link(systemPath),
	))
}

func (s *Service) GetSystem(c *gin.Context) {
	c.JSON(http.StatusOK, buildSystemResource())
}

func (s *Service) ResetSystem(c *gin.Context) {
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

	ctrl := power.GetController()
	var err error

	switch req.ResetType {
	case schemas.OnResetType:
		err = ctrl.PowerOn()
	case schemas.ForceOffResetType, schemas.GracefulShutdownResetType:
		err = ctrl.PowerOff()
	case schemas.ForceRestartResetType, schemas.PowerCycleResetType:
		err = ctrl.Reset()
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

	log.Debugf("redfish reset action: %s", req.ResetType)
	c.Status(http.StatusNoContent)
}

func (s *Service) PatchSystem(c *gin.Context) {
	var req struct {
		Boot struct {
			BootSourceOverrideTarget  schemas.BootSource                `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled schemas.BootSourceOverrideEnabled `json:"BootSourceOverrideEnabled"`
			// Mode is accepted but ignored — the RPi5 firmware path is
			// UEFI-only, so there is no toggle to honour. buildSystemResource
			// echoes it back so PATCH responses stay consistent.
			BootSourceOverrideMode schemas.BootSourceOverrideMode `json:"BootSourceOverrideMode"`
		} `json:"Boot"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body")
		return
	}

	target := req.Boot.BootSourceOverrideTarget
	enabled := req.Boot.BootSourceOverrideEnabled
	if enabled == "" {
		enabled = schemas.OnceBootSourceOverrideEnabled // Redfish convention
	}

	// Disabled clears the override regardless of target.
	if enabled == schemas.DisabledBootSourceOverrideEnabled || target == schemas.NoneBootSource {
		clearBootOverride()
		log.Debugf("redfish boot override cleared")
		c.JSON(http.StatusOK, buildSystemResource())
		return
	}

	if !bootSourceSupported(target) {
		redfishErrorResponse(c, http.StatusBadRequest,
			"invalid BootSourceOverrideTarget: "+string(target))
		return
	}

	if err := setBootOverride(target, enabled); err != nil {
		log.Warnf("redfish: boot override write failed: %v", err)
	}

	log.Debugf("redfish boot override: target=%s enabled=%s", target, enabled)
	c.JSON(http.StatusOK, buildSystemResource())
}

func buildSystemResource() ComputerSystem {
	powerState := schemas.OffPowerState
	if on, err := power.GetController().State(); err == nil && on {
		powerState = schemas.OnPowerState
	}

	biosLink := Link(biosPath)
	memoryLink := Link(memoryPath)
	processorsLink := Link(processorsPath)
	storageLink := Link(storageRootPath)
	nicsLink := Link(ethernetInterfacesPath)
	sys := ComputerSystem{
		Resource: Resource{
			ODataType:    "#ComputerSystem.v1_13_0.ComputerSystem",
			ODataID:      systemPath,
			ODataContext: context("ComputerSystem.ComputerSystem"),
			ID:           "1",
			Name:         "Computer System",
		},
		SystemType: schemas.PhysicalSystemType,
		PowerState: powerState,
		Boot:       readBoot(),
		// Bios points the client at the EEPROM configuration surface (see
		// bios.go). Standard navigation property — clients follow @odata.id
		// to GET the current bootloader settings.
		Bios: &biosLink,
		// Per-device inventory collections. Always linked (JetKVM-style):
		// clients descend and find whatever the current SMBIOS/env/gadget
		// state provides, which may be an empty collection pre-first-boot.
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
		// The rpi-eeprom bootloader is exposed as a TrustedComponent (the
		// platform root of trust); its version/flash-time live on the nested
		// SoftwareInventory. See trusted_components.go.
		Links: &SystemLinks{
			TrustedComponents: Links{Link(bootloaderComponentPath)},
			Chassis:           Links{Link(chassisItemPath)},
		},
	}

	// Environment first, SMBIOS overlaid on top — see inventory.go.
	applyEnvInventory(&sys)
	applySMBIOSInventory(&sys)

	return sys
}
