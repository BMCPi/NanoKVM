// Package firmware exposes the firmware API: FMP capsule staging, virtual
// media, and USB gadget presentation.
//
// The BMC no longer serves a bootable host image over the gadget. Firmware is
// delivered as UEFI FMP capsules: they are staged into \EFI\UpdateCapsule\ on
// the capsule volume presented over USB mass storage, and the host's own
// firmware finds and applies them at its next boot (UEFI 2.10 §8.5.5).
//
// Boot overrides and host inventory live on the Redfish surface (api/redfish)
// — the host's firmware reads and applies them itself.
package firmware

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// maxCapsuleUploadBytes caps a capsule upload. Capsules are firmware-sized;
// uploads are streamed into the capsule volume (constant memory), so this
// bounds what a client can force onto the volume rather than RAM.
const maxCapsuleUploadBytes = 128 << 20 // 128 MiB

// maxMediaUploadBytes caps an ISO upload. Like capsules, media uploads stream
// straight to the data partition, so this bounds disk rather than RAM.
const maxMediaUploadBytes = 8 << 30 // 8 GiB

// capsuleStageTimeout bounds a BMC-initiated capsule download. Generous
// because the link to the capsule host may be slow and a capsule is
// firmware-sized, but finite so a stalled transfer cannot hold the
// "a capsule is being staged" latch until the next reboot.
const capsuleStageTimeout = 30 * time.Minute

// errorKey is the JSON field every failure response in this file uses.
const errorKey = "error"

// handlers holds what a route handler in this package needs:
// process-lifetime dependencies (through d) and this package's component
// logger.
type handlers struct {
	d   *deps.Deps
	log *slog.Logger
}

// Register mounts the firmware routes on the shared authenticated group.
func Register(api *gin.RouterGroup, d *deps.Deps) {
	h := &handlers{d: d, log: logger.Or(d.Log).With("component", "api/firmware")}
	ctrl := d.Firmware
	fw := api.Group("/firmware")

	registerCapsules(fw, h, ctrl)
	registerMedia(fw, ctrl)
	registerGadget(fw, ctrl)
}

// registerCapsules wires capsule staging: what is queued for the host, and the
// three ways to change that (upload, fetch from a URL, delete).
func registerCapsules(fw *gin.RouterGroup, h *handlers, ctrl *firmware.Controller) {
	// GET /api/firmware/status — capsule volume state plus what is staged.
	fw.GET("/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, ctrl.GetStatus())
	})

	// GET /api/firmware/capsules — the capsules the host will find at boot.
	fw.GET("/capsules", func(c *gin.Context) {
		capsules, err := ctrl.ListCapsules()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"capsules": capsules})
	})

	// POST /api/firmware/capsules — upload a capsule. Accepts multipart form
	// field "file" or a raw binary body with ?name=<filename>.
	fw.POST("/capsules", func(c *gin.Context) {
		if ctrl.IsStaging() {
			c.JSON(http.StatusConflict, gin.H{errorKey: "a capsule is already being staged"})
			return
		}

		var (
			src  io.Reader
			name string
		)
		if strings.HasPrefix(c.ContentType(), "multipart/") {
			// Streamed part-by-part; c.FormFile would spool the whole body
			// into the RAM-backed os.TempDir() first. See
			// pkg/utils/multipart_stream.go.
			upload, err := utils.StreamMultipartFile(c.Request, maxCapsuleUploadBytes, "file")
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{errorKey: "multipart field 'file' required"})
				return
			}
			defer upload.Close()
			src = upload
			name = filepath.Base(upload.Filename)
		} else {
			src = http.MaxBytesReader(c.Writer, c.Request.Body, maxCapsuleUploadBytes)
			name = filepath.Base(c.Query("name"))
		}
		if name == "" || name == "." || name == "/" {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "capsule filename required (multipart filename or ?name=)"})
			return
		}

		written, err := ctrl.StageCapsule(name, src)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"capsule": name, "bytes": written})
	})

	// POST /api/firmware/capsules/fetch — have the BMC download a capsule.
	// Body: { "url": "https://…/host.cap", "name": "…" (optional) }
	fw.POST("/capsules/fetch", func(c *gin.Context) {
		var req struct {
			URL  string `json:"url"`
			Name string `json:"name"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.URL == "" {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "url required"})
			return
		}
		if parsed, err := url.ParseRequestURI(req.URL); err != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "url must be http or https"})
			return
		}
		if ctrl.IsStaging() {
			c.JSON(http.StatusConflict, gin.H{errorKey: "a capsule is already being staged"})
			return
		}

		// Detached from the request but bounded by the process context, so a
		// shutdown aborts the transfer. See deps.ActionContext.
		ctx, cancel := h.d.ActionContext(capsuleStageTimeout)
		go func(rawURL, name string) {
			defer cancel()
			if err := ctrl.StageCapsuleFromURL(ctx, rawURL, name); err != nil {
				h.log.ErrorContext(ctx, "firmware: capsule fetch failed", slog.Any("err", err))
			}
		}(req.URL, filepath.Base(req.Name))

		c.JSON(http.StatusAccepted, gin.H{"message": "capsule fetch started"})
	})

	// DELETE /api/firmware/capsules/:name — un-stage a capsule the host has
	// not consumed yet.
	fw.DELETE("/capsules/:name", func(c *gin.Context) {
		name := filepath.Base(c.Param("name"))
		if err := ctrl.RemoveCapsule(name); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"capsule": name, "deleted": true})
	})

	// POST /api/firmware/capsules/clear — cancel every pending update.
	fw.POST("/capsules/clear", func(c *gin.Context) {
		if err := ctrl.ClearCapsules(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "cleared"})
	})
}

// registerMedia wires the virtual media (ISO) management endpoints.
func registerMedia(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	// GET /api/firmware/media — list staged ISOs and current insertion state.
	fw.GET("/media", func(c *gin.Context) {
		names, err := ctrl.ListMediaFiles()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{errorKey: err.Error()})
			return
		}
		vm := ctrl.GetVirtualMediaState()
		c.JSON(http.StatusOK, gin.H{
			"files":    names,
			"inserted": vm.ImageName,
			"state":    vm,
		})
	})

	// POST /api/firmware/media/upload — save an ISO to the staging directory
	// (multipart form field "file"). Does not insert; call /insert after.
	//
	// Streamed part-by-part rather than via c.FormFile: FormFile spools the
	// whole body into os.TempDir(), which is the RAM-backed overlay on this
	// device. See pkg/utils/multipart_stream.go.
	fw.POST("/media/upload", func(c *gin.Context) {
		upload, err := utils.StreamMultipartFile(c.Request, maxMediaUploadBytes, "file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "multipart field 'file' required"})
			return
		}
		defer upload.Close()

		name := filepath.Base(upload.Filename)

		// Sniff for gzip/xz/zstd and inflate on the fly; an uncompressed ISO
		// passes through unchanged. maxMediaUploadBytes already bounds the
		// wire via StreamMultipartFile above — LimitDecompressedReader
		// re-bounds the same budget on the inflated side, since a compressed
		// part can expand far past it.
		dr, format, err := utils.DecompressingReader(upload)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "decompress failed: " + err.Error()})
			return
		}
		defer dr.Close()
		name = utils.StripCompressionSuffix(name, format)

		n, err := ctrl.SaveMediaFile(name, utils.LimitDecompressedReader(dr, maxMediaUploadBytes))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"file": name, "bytes": n})
	})

	// POST /api/firmware/media/fetch — download an ISO from a URL into the
	// staging directory. Body: { "url": "https://…/image.iso", "name": "…" (optional) }
	fw.POST("/media/fetch", func(c *gin.Context) {
		var req struct {
			URL  string `json:"url"`
			Name string `json:"name"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.URL == "" {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "url required"})
			return
		}
		parsed, err := url.ParseRequestURI(req.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "url must be http or https"})
			return
		}
		name := req.Name
		if name == "" {
			name = filepath.Base(parsed.Path)
		}
		name = filepath.Base(name)
		if name == "." || name == "" || strings.ContainsAny(name, "/\\") {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "invalid filename derived from URL"})
			return
		}
		// Bounded and timeout-guarded; the body streams straight to the media
		// directory on the data partition.
		remote, err := utils.FetchURL(c.Request.Context(), req.URL, maxMediaUploadBytes)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{errorKey: "fetch failed: " + err.Error()})
			return
		}
		defer remote.Close()

		// Sniff for gzip/xz/zstd and inflate on the fly; an uncompressed ISO
		// passes through unchanged. maxMediaUploadBytes already bounds the
		// wire via FetchURL above — LimitDecompressedReader re-bounds the
		// same budget on the inflated side, since a compressed body can
		// expand far past it.
		dr, format, err := utils.DecompressingReader(remote)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{errorKey: "decompress failed: " + err.Error()})
			return
		}
		defer dr.Close()
		name = utils.StripCompressionSuffix(name, format)

		n, err := ctrl.SaveMediaFile(name, utils.LimitDecompressedReader(dr, maxMediaUploadBytes))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"file": name, "bytes": n})
	})

	// POST /api/firmware/media/insert — copy a staged ISO into the firmware
	// image as vm.iso and set the usb boot target.
	// Body: { "name": "alpine.iso" }
	fw.POST("/media/insert", func(c *gin.Context) {
		var req struct {
			Name string `json:"name"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{errorKey: "name required"})
			return
		}
		if err := ctrl.InsertVirtualMedia(req.Name); err != nil {
			c.JSON(http.StatusConflict, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, ctrl.GetVirtualMediaState())
	})

	// DELETE /api/firmware/media/:name — remove a staged ISO (must not be inserted).
	fw.DELETE("/media/:name", func(c *gin.Context) {
		name := filepath.Base(c.Param("name"))
		if err := ctrl.DeleteMediaFile(name); err != nil {
			c.JSON(http.StatusConflict, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"file": name, "deleted": true})
	})

	// POST /api/firmware/media/eject — clear lun.1 so the host sees an empty
	// CD-ROM tray. The staged ISO stays in mediaDir.
	fw.POST("/media/eject", func(c *gin.Context) {
		if err := ctrl.EjectVirtualMedia(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, ctrl.GetVirtualMediaState())
	})
}

// registerGadget wires the USB gadget presentation controls for lun.0 (the
// capsule volume). Unpresenting hides the volume from the host entirely; it is
// re-presented automatically around every capsule write.
func registerGadget(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	fw.POST("/present", func(c *gin.Context) {
		if err := ctrl.Present(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "presented"})
	})

	fw.POST("/unpresent", func(c *gin.Context) {
		if err := ctrl.Unpresent(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "unpresented"})
	})
}
