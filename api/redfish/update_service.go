package redfish

// update_service.go implements a minimal Redfish UpdateService: a
// FirmwareInventory whose BIOS entry reports the version the host itself
// reported, plus a SimpleUpdate action that downloads a host boot image
// from a caller-supplied URL and presents it on the USB gadget. The BMC
// does not flash anything — the image is transport, consumed by the host.
//
// StartUpdate is gone deliberately: its "no parameters, fetch latest"
// semantics regressed hosts to stale images by design.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"
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

// GetFirmwareInventoryBIOS returns the host boot firmware inventory entry.
// The version is whatever the host last reported about itself — the BMC has
// no other window into what the host is actually running.
func (s *Service) GetFirmwareInventoryBIOS(c *gin.Context) {
	reported, _ := HostReported()
	version := reported.BiosVersion
	if version == "" {
		version = "Unknown"
	}

	c.JSON(http.StatusOK, SoftwareInventory{
		Resource: Resource{
			ODataType:    "#SoftwareInventory.v1_8_0.SoftwareInventory",
			ODataID:      firmwareBIOSPath,
			ODataContext: context("SoftwareInventory.SoftwareInventory"),
			ID:           "BIOS",
			Name:         "BIOS",
			Description:  "Host boot firmware version, as reported by the host",
		},
		SoftwareID: "BIOS",
		Version:    version,
		Updateable: true,
		Status:     &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	})
}

// SimpleUpdate downloads the image at ImageURI and presents it to the host
// on the USB gadget. ImageURI is required: there is no implicit "latest".
func (s *Service) SimpleUpdate(c *gin.Context) {
	var req struct {
		ImageURI         string   `json:"ImageURI"`
		TransferProtocol string   `json:"TransferProtocol"`
		Targets          []string `json:"Targets"`
	}
	_ = c.ShouldBindJSON(&req) // remaining fields optional

	if req.ImageURI == "" {
		redfishErrorResponse(c, http.StatusBadRequest, "ImageURI is required")
		return
	}

	ctrl := s.Firmware
	if ctrl.IsDownloading() {
		redfishErrorResponse(c, http.StatusConflict, "update already in progress")
		return
	}

	go func(url string) {
		if err := ctrl.UpdateHostImageFromURL(url); err != nil {
			log.Errorf("redfish: host image update failed: %v", err)
		}
	}(req.ImageURI)

	c.JSON(http.StatusAccepted, Message{
		ODataType: "#Message.v1_1_0.Message",
		MessageID: "Update.1.0.UpdateInProgress",
		Message:   "Host image update started",
		Severity:  "OK",
	})
}
