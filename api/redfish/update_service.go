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
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// capsuleStageTimeout bounds a BMC-initiated capsule download started by
// SimpleUpdate. Generous because the link may be slow and a capsule is
// firmware-sized, but finite so a stalled transfer cannot hold the
// "a capsule is being staged" latch until the next reboot.
const capsuleStageTimeout = 30 * time.Minute

// maxCapsulePushBytes caps an HttpPushUri body. Capsules are firmware-sized;
// this bounds what a client can force onto the capsule volume.
const maxCapsulePushBytes = 128 << 20 // 128 MiB

// GetUpdateService returns the UpdateService root.
func (s *Service) GetUpdateService(c *gin.Context) {
	c.JSON(http.StatusOK, UpdateService{
		Resource: Resource{
			ODataType:    "#UpdateService.v1_11_0.UpdateService",
			ODataID:      updateServicePath,
			ODataContext: odataContext("UpdateService.UpdateService"),
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

// GetFirmwareInventoryCollection returns the firmware inventory collection:
// the members the host has PATCHed, or the synthesized BiosFirmware entry
// before the first report so the branch is never empty.
func (s *Service) GetFirmwareInventoryCollection(c *gin.Context) {
	links := Links{}
	for _, id := range hostCollectionIDs(firmwareOf) {
		links = append(links, Link(firmwareInventoryPath+"/"+id))
	}
	if len(links) == 0 {
		links = Links{Link(firmwareBiosFirmwarePath)}
	}
	c.JSON(http.StatusOK, newCollection(
		"SoftwareInventoryCollection", "Firmware Inventory Collection", firmwareInventoryPath,
		links...,
	))
}

// GetFirmwareInventoryMember returns one firmware inventory entry: the
// SoftwareInventory document the host PATCHed if there is one, else — for the
// BiosFirmware member and its legacy "BIOS" spelling — a minimal entry built
// from the BiosVersion the host reported on Systems/1. The BMC has no other
// window into what the host is actually running.
func (s *Service) GetFirmwareInventoryMember(c *gin.Context) {
	id := c.Param("id")
	path := firmwareInventoryPath + "/" + id

	if stored, ok := hostCollectionGet(firmwareOf, id); ok {
		writeHostResource(c, renderHostMember(stored, path, id,
			"#SoftwareInventory.v1_2_3.SoftwareInventory",
			"SoftwareInventory.SoftwareInventory", id))
		return
	}
	if id != firmwareBiosMemberID && id != firmwareBiosLegacyID {
		redfishErrorResponse(c, http.StatusNotFound, "no such firmware inventory member")
		return
	}

	reported, _ := HostReported()
	version := reported.BiosVersion
	if version == "" {
		version = "Unknown"
	}
	c.JSON(http.StatusOK, SoftwareInventory{
		Resource: Resource{
			ODataType:    "#SoftwareInventory.v1_8_0.SoftwareInventory",
			ODataID:      path,
			ODataContext: odataContext("SoftwareInventory.SoftwareInventory"),
			ID:           id,
			Name:         "BIOS",
			Description:  "Host boot firmware version, as reported by the host",
		},
		SoftwareID: "BIOS",
		Version:    version,
		Updateable: true,
		Status:     &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	})
}

// PatchFirmwareInventoryMember stores the host's SoftwareInventory report
// (RpiRedfishSyncDxe PATCHes member "BiosFirmware" once per boot: version,
// the ESRT class GUID as SoftwareId, and the FMP integers under Oem.PiBmc).
// PATCH merges per DSP0266; the host re-reports the full document each boot
// so the merged result tracks it, while a boot that omits LastAttempt* keeps
// the last known attempt visible.
func (s *Service) PatchFirmwareInventoryMember(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	id := c.Param("id")
	path := firmwareInventoryPath + "/" + id
	if current, ok := hostCollectionGet(firmwareOf, id); ok {
		if !hostCheckIfMatch(c, renderHostMember(current, path, id,
			"#SoftwareInventory.v1_2_3.SoftwareInventory",
			"SoftwareInventory.SoftwareInventory", id)) {
			return
		}
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	// Upsert: PATCH is the only verb the host uses here, so the first report
	// of a boot creates the member (hostCollectionMerge only updates ones
	// that already exist).
	merged := hostCollectionMerge(firmwareOf, id, body)
	if merged == nil {
		hostCollectionPut(firmwareOf, id, body)
		merged = body
	}
	writeHostResource(c, renderHostMember(merged, path, id,
		"#SoftwareInventory.v1_2_3.SoftwareInventory",
		"SoftwareInventory.SoftwareInventory", id))
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

	// Detached from the request — the 202 below returns immediately and the
	// download runs on past it — but NOT from the process: the context comes
	// from deps, so SIGTERM aborts a transfer in flight instead of leaving it
	// to be killed mid-write. See deps.ActionContext.
	ctx, cancel := s.Deps.ActionContext(capsuleStageTimeout)
	go func(url string) {
		defer cancel()
		if err := ctrl.StageCapsuleFromURL(ctx, url, ""); err != nil {
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
