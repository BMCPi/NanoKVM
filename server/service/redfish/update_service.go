package redfish

// update_service.go implements a minimal Redfish UpdateService surface
// for the U-Boot firmware: FirmwareInventory listing the current/latest
// versions, plus a SimpleUpdate action that triggers an in-place
// download from the pi-bmc/firmware-images GitHub releases.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/server/service/firmware"
	"github.com/pi-bmc/nanokvm-app/server/service/smbios"
)

// GetUpdateService returns the UpdateService root.
func (s *Service) GetUpdateService(c *gin.Context) {
	c.JSON(http.StatusOK, UpdateService{
		Resource: Resource{
			ODataType:    "#UpdateService.v1_11_0.UpdateService",
			ODataID:      updateServicePath,
			ODataContext: context("UpdateService.UpdateService"),
			ID:           "UpdateService",
			Name:         "Update Service",
		},
		ServiceEnabled:    true,
		FirmwareInventory: Link(firmwareInventoryPath),
		Actions: UpdateServiceActions{
			SimpleUpdate: SimpleUpdateAction{
				Target:                    simpleUpdatePath,
				AllowableTransferProtocol: []string{"HTTPS"},
			},
			StartUpdate: ActionTarget{Target: startUpdatePath},
		},
	})
}

// GetFirmwareInventoryCollection returns the firmware inventory collection.
func (s *Service) GetFirmwareInventoryCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"SoftwareInventoryCollection", "Firmware Inventory Collection", firmwareInventoryPath,
		Link(firmwareBIOSPath),
	))
}

// GetFirmwareInventoryUBoot returns the U-Boot firmware inventory entry
// (exposed under the "BIOS" id since U-Boot serves the BIOS role here).
func (s *Service) GetFirmwareInventoryUBoot(c *gin.Context) {
	ctrl := firmware.GetController()
	info, err := ctrl.GetUBootVersionInfo()
	current := info.Current
	if current == "" {
		current = "Unknown"
	}

	description := "U-Boot bootloader firmware"
	if err != nil {
		description += " (latest-version lookup failed: " + err.Error() + ")"
	}

	resp := SoftwareInventory{
		Resource: Resource{
			ODataType:    "#SoftwareInventory.v1_8_0.SoftwareInventory",
			ODataID:      firmwareBIOSPath,
			ODataContext: context("SoftwareInventory.SoftwareInventory"),
			ID:           "BIOS",
			Name:         "BIOS (U-Boot)",
			Description:  description,
		},
		SoftwareID: "U-Boot",
		Version:    current,
		Updateable: true,
		Status:     &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	}
	// SMBIOS type 0 carries the firmware vendor and build date — the
	// standard SoftwareInventory members for what used to sit under
	// Oem.NanoKVM as BIOSVendor/BIOSDate on the ComputerSystem.
	if info2, err := smbios.GetStore().Load(); err == nil && info2 != nil {
		resp.Manufacturer = info2.BIOSVendor
		resp.ReleaseDate = info2.BIOSDate
	}
	if info.Latest != "" {
		resp.Oem = Oem{
			"BMCPi": map[string]any{
				"LatestVersion":   info.Latest,
				"UpdateAvailable": info.UpdateAvailable,
				"AssetURL":        info.AssetURL,
			},
		}
	}
	c.JSON(http.StatusOK, resp)
}

// SimpleUpdate triggers a u-boot update from a provided ImageURI (or the
// latest GitHub release if omitted).
func (s *Service) SimpleUpdate(c *gin.Context) {
	var req struct {
		ImageURI         string   `json:"ImageURI"`
		TransferProtocol string   `json:"TransferProtocol"`
		Targets          []string `json:"Targets"`
	}
	_ = c.ShouldBindJSON(&req) // all fields optional

	ctrl := firmware.GetController()
	if ctrl.IsDownloading() {
		redfishErrorResponse(c, http.StatusConflict, "update already in progress")
		return
	}

	go func(url string) {
		var err error
		if url != "" {
			err = ctrl.UpdateUBootFromURL(url)
		} else {
			err = ctrl.UpdateUBoot()
		}
		if err != nil {
			log.Errorf("redfish: u-boot update failed: %v", err)
		}
	}(req.ImageURI)

	c.JSON(http.StatusAccepted, Message{
		ODataType: "#Message.v1_1_0.Message",
		MessageID: "Update.1.0.UpdateInProgress",
		Message:   "U-Boot update started",
		Severity:  "OK",
	})
}

// StartUpdate is an alias for SimpleUpdate with no parameters: always
// fetches the latest release from GitHub.
func (s *Service) StartUpdate(c *gin.Context) {
	ctrl := firmware.GetController()
	if ctrl.IsDownloading() {
		redfishErrorResponse(c, http.StatusConflict, "update already in progress")
		return
	}
	go func() {
		if err := ctrl.UpdateUBoot(); err != nil {
			log.Errorf("redfish: u-boot update failed: %v", err)
		}
	}()
	c.JSON(http.StatusAccepted, Message{
		ODataType: "#Message.v1_1_0.Message",
		MessageID: "Update.1.0.UpdateInProgress",
		Message:   "U-Boot update started",
		Severity:  "OK",
	})
}
