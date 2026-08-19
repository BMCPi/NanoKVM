package redfish

// update_service.go implements a minimal Redfish UpdateService: a
// FirmwareInventory whose BIOS entry reports the version the host itself
// reported, plus the two standard ways of handing the BMC a UEFI FMP capsule —
// SimpleUpdate (the BMC fetches it from a URL) and an HttpPushUri (the client
// POSTs the bytes).
//
// Neither path flashes the host. Both stage the capsule into
// \EFI\UpdateCapsule\ on the capsule volume the BMC presents over USB mass
// storage; the host's own firmware finds it at its next boot and applies it
// via FMP (UEFI 2.10 §8.5.5, "Delivering Capsules Across a System Reset").
// Serving a bootable host image over the gadget is retired — that transport no
// longer exists, and neither does StartUpdate, whose "no parameters, fetch
// latest" semantics regressed hosts to stale images by design.

import (
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// maxCapsulePushBytes caps an HttpPushUri body. Capsules are firmware-sized;
// this bounds what a client can force onto the capsule volume.
const maxCapsulePushBytes = 128 << 20 // 128 MiB

// GetUpdateService returns the UpdateService root.
func (s *Service) GetUpdateService(c *gin.Context) {
	c.JSON(http.StatusOK, UpdateService{
		Resource: Resource{
			ODataType:    "#UpdateService.v1_11_0.UpdateService",
			ODataID:      updateServicePath,
			ODataContext: context("UpdateService.UpdateService"),
			ID:           "UpdateService",
			Name:         "Update Service",
			Description:  "Stages UEFI FMP capsules for the managed host to apply at its next boot",
		},
		ServiceEnabled:    true,
		FirmwareInventory: Link(firmwareInventoryPath),
		HTTPPushURI:       httpPushURIPath,
		Actions: UpdateServiceActions{
			SimpleUpdate: SimpleUpdateAction{
				Target:                    simpleUpdatePath,
				AllowableTransferProtocol: []string{"HTTP", "HTTPS"},
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

// SimpleUpdate downloads the FMP capsule at ImageURI and stages it on the
// capsule volume. ImageURI is required: there is no implicit "latest".
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
	if ctrl.IsStaging() {
		redfishErrorResponse(c, http.StatusConflict, "a capsule is already being staged")
		return
	}

	go func(url string) {
		if err := ctrl.StageCapsuleFromURL(url, ""); err != nil {
			log.Errorf("redfish: capsule staging failed: %v", err)
		}
	}(req.ImageURI)

	c.JSON(http.StatusAccepted, Message{
		ODataType: "#Message.v1_1_0.Message",
		MessageID: "Update.1.0.UpdateInProgress",
		Message:   "Capsule staging started; the host applies it at its next boot",
		Severity:  "OK",
	})
}

// PushCapsule is the HttpPushUri handler: the client POSTs the capsule bytes
// and the BMC stages them directly, no outbound fetch involved. Accepts a raw
// body (application/octet-stream) or multipart form field "UpdateFile" —
// Redfish's MultipartHttpPushUri field name — falling back to "file".
func (s *Service) PushCapsule(c *gin.Context) {
	ctrl := s.Firmware
	if ctrl.IsStaging() {
		redfishErrorResponse(c, http.StatusConflict, "a capsule is already being staged")
		return
	}

	var (
		src  io.Reader
		name string
	)
	if strings.HasPrefix(c.ContentType(), "multipart/") {
		// Streamed part-by-part: c.FormFile spools the whole body into
		// os.TempDir() before returning, and on this device that is the
		// RAM-backed root overlay — far smaller than a capsule is allowed to
		// be, so the server died mid-push. See pkg/utils/multipart_stream.go.
		upload, err := utils.StreamMultipartFile(c.Request, maxCapsulePushBytes, "UpdateFile", "file")
		if err != nil {
			redfishErrorResponse(c, http.StatusBadRequest, "multipart field 'UpdateFile' required")
			return
		}
		defer upload.Close()
		src = upload
		name = path.Base(upload.Filename)
	} else {
		src = http.MaxBytesReader(c.Writer, c.Request.Body, maxCapsulePushBytes)
		// No filename in a raw push; let the caller name it with ?name= and
		// fall back to a fixed one so repeated pushes replace rather than pile
		// up on a volume the host only drains at boot.
		name = path.Base(c.Query("name"))
	}
	if name == "" || name == "." || name == "/" {
		name = "update.cap"
	}

	written, err := ctrl.StageCapsule(name, src)
	if err != nil {
		redfishErrorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}
	log.Infof("redfish: staged pushed capsule %s (%d bytes)", name, written)

	c.JSON(http.StatusAccepted, Message{
		ODataType: "#Message.v1_1_0.Message",
		MessageID: "Update.1.0.AwaitToUpdate",
		Message:   "Capsule staged; the host applies it at its next boot",
		Severity:  "OK",
	})
}
