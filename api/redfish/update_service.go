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
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/streamio"
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
func (h *handlers) GetUpdateService(c *gin.Context) {
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
// exactly the members the host has PATCHed. Empty before the first report —
// no synthesized placeholder — because the member id is no longer pinned to
// a compiled-in name (a RPi host reports "BiosFirmware"; a NUC reports its
// own), so there is nothing honest to synthesize before the host says what
// it has.
func (h *handlers) GetFirmwareInventoryCollection(c *gin.Context) {
	links := Links{}
	for _, id := range hostCollectionIDs(firmwareOf) {
		links = append(links, Link(firmwareInventoryPath+"/"+id))
	}
	c.JSON(http.StatusOK, newCollection(
		"SoftwareInventoryCollection", "Firmware Inventory Collection", firmwareInventoryPath,
		links...,
	))
}

// GetFirmwareInventoryMember returns one firmware inventory entry: the
// SoftwareInventory document the host PATCHed, or a 404 — the same as any
// other host-reported collection. There is no synthesized fallback member
// (formerly pinned to "BiosFirmware"/"BIOS"): a member id is whatever the
// host's own firmware chooses to report.
func (h *handlers) GetFirmwareInventoryMember(c *gin.Context) {
	id := c.Param("id")
	resPath := firmwareInventoryPath + "/" + id

	stored, ok := hostCollectionGet(firmwareOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "no such firmware inventory member")
		return
	}
	writeHostResource(c, renderHostMember(stored, resPath, id,
		"#SoftwareInventory.v1_2_3.SoftwareInventory",
		"SoftwareInventory.SoftwareInventory", id))
}

// PatchFirmwareInventoryMember stores the host's SoftwareInventory report
// (RpiRedfishSyncDxe PATCHes member "BiosFirmware" once per boot: version,
// the ESRT class GUID as SoftwareId, and the FMP integers under Oem.PiBmc).
// PATCH merges per DSP0266; the host re-reports the full document each boot
// so the merged result tracks it, while a boot that omits LastAttempt* keeps
// the last known attempt visible.
func (h *handlers) PatchFirmwareInventoryMember(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	id := c.Param("id")
	resPath := firmwareInventoryPath + "/" + id
	if current, ok := hostCollectionGet(firmwareOf, id); ok {
		if !hostCheckIfMatch(c, renderHostMember(current, resPath, id,
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
	writeHostResource(c, renderHostMember(merged, resPath, id,
		"#SoftwareInventory.v1_2_3.SoftwareInventory",
		"SoftwareInventory.SoftwareInventory", id))
}

// SimpleUpdate downloads the FMP capsule at ImageURI and stages it on the
// capsule volume. ImageURI is required: there is no implicit "latest".
func (h *handlers) SimpleUpdate(c *gin.Context) {
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

	ctrl := h.d.Firmware
	if ctrl.IsStaging() {
		redfishErrorResponse(c, http.StatusConflict, "a capsule is already being staged")
		return
	}

	// The task is what turns the old fire-and-forget 202 into something an
	// operator tool (Ansible redfish_command, gofish, redfishtool) can poll:
	// Location points at it, and the goroutine below drives it to
	// Completed/Exception. The old bare-202 body's UpdateInProgress message
	// is folded into Task.Messages so no information is lost.
	t := h.tasks.newTask("SimpleUpdate: stage capsule")
	t.addMessage(Message{
		ODataType: "#Message.v1_1_0.Message",
		MessageID: "Update.1.0.UpdateInProgress",
		Message:   "Capsule staging started; the host applies it at its next boot",
		Severity:  "OK",
	})

	// Detached from the request — the 202 below returns immediately and the
	// download runs on past it — but NOT from the process: the context comes
	// from deps, so SIGTERM aborts a transfer in flight instead of leaving it
	// to be killed mid-write. See deps.ActionContext.
	ctx, cancel := h.d.ActionContext(capsuleStageTimeout)
	go func(url string) {
		defer cancel()
		err := ctrl.StageCapsuleFromURL(ctx, url, "",
			firmware.WithProgress(func(loaded, total int64) {
				if total <= 0 {
					return // no declared length: leave PercentComplete unset
				}
				t.setPercent(int(loaded * 100 / total))
			}))
		if err != nil {
			h.log.ErrorContext(ctx, "redfish: capsule staging failed", slog.Any("err", err))
		}
		t.complete(err)
	}(req.ImageURI)

	acceptedTask(c, t)
}

// PushCapsule is the HttpPushUri handler: the client POSTs the capsule bytes
// and the BMC stages them directly, no outbound fetch involved. Accepts a raw
// body (application/octet-stream) or multipart form field "UpdateFile" —
// Redfish's MultipartHttpPushUri field name — falling back to "file".
func (h *handlers) PushCapsule(c *gin.Context) {
	ctrl := h.d.Firmware
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
		// be, so the server died mid-push. See pkg/streamio/multipart_stream.go.
		upload, err := streamio.StreamMultipartFile(c.Request, maxCapsulePushBytes, "UpdateFile", "file")
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
	h.log.InfoContext(c.Request.Context(), "redfish: staged pushed capsule",
		slog.String("name", name), slog.Int64("bytes", written))

	c.JSON(http.StatusAccepted, Message{
		ODataType: "#Message.v1_1_0.Message",
		MessageID: "Update.1.0.AwaitToUpdate",
		Message:   "Capsule staged; the host applies it at its next boot",
		Severity:  "OK",
	})
}
