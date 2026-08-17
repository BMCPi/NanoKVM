// Package firmware exposes the firmware API: image status and download,
// direct FAT file management, virtual media, and USB gadget presentation.
// Boot overrides and host inventory live on the Redfish surface
// (api/redfish) — the host's firmware reads and applies them itself.
package firmware

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// Upload body cap. Uploads are streamed to the image (constant memory), so
// this bounds the on-image/disk size against abuse rather than RAM.
const maxFileUploadBytes = 128 << 20 // 128 MiB — boot-partition files

// Register mounts the firmware routes on the shared authenticated group.
func Register(api *gin.RouterGroup, d *deps.Deps) {
	ctrl := d.Firmware
	fw := api.Group("/firmware")

	registerImage(fw, ctrl)
	registerFiles(fw, ctrl)
	registerMedia(fw, ctrl)
	registerGadget(fw, ctrl)
}

// registerImage wires image status and download.
func registerImage(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	fw.GET("/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, ctrl.GetStatus())
	})

	fw.POST("/download", func(c *gin.Context) {
		if ctrl.IsDownloading() {
			c.JSON(http.StatusConflict, gin.H{"error": "download already in progress"})
			return
		}

		go func() {
			if err := ctrl.DownloadAndInit(); err != nil {
				log.Errorf("firmware download failed: %v", err)
			}
		}()

		c.JSON(http.StatusAccepted, gin.H{"message": "download started"})
	})
}

// registerFiles wires the direct FAT file management endpoints (go-diskfs).
func registerFiles(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	// GET /api/firmware/files — list all files in the FAT root.
	fw.GET("/files", func(c *gin.Context) {
		names, err := ctrl.ListFilesInImage()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"files": names})
	})

	// GET /api/firmware/file/:name — download a file from the FAT image.
	fw.GET("/file/:name", func(c *gin.Context) {
		name := filepath.Base(c.Param("name")) // sanitise; stay at root
		data, err := ctrl.ReadFileFromImage(name)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		if data == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("%s not found in image", name)})
			return
		}
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		c.Data(http.StatusOK, "application/octet-stream", data)
	})

	// PUT /api/firmware/file/:name — upload / overwrite a file in the FAT image.
	// Accepts raw binary body (Content-Type: application/octet-stream) or
	// multipart form field "file".
	fw.PUT("/file/:name", func(c *gin.Context) {
		name := filepath.Base(c.Param("name"))

		// Stream the upload straight into the image so memory stays flat
		// regardless of file size (see firmware.WriteReaderToImage). The body is
		// capped to guard the image/disk, not RAM.
		var src io.Reader
		ct := c.ContentType()
		if ct == "multipart/form-data" {
			fh, err := c.FormFile("file")
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "multipart field 'file' required"})
				return
			}
			f, err := fh.Open()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer f.Close()
			src = io.LimitReader(f, maxFileUploadBytes)
		} else {
			src = http.MaxBytesReader(c.Writer, c.Request.Body, maxFileUploadBytes)
		}

		written, err := ctrl.WriteReaderToImage(name, src)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"file": name, "bytes": written})
	})

	// DELETE /api/firmware/file/:name — remove a file from the FAT image.
	fw.DELETE("/file/:name", func(c *gin.Context) {
		name := filepath.Base(c.Param("name"))
		if err := ctrl.RemoveFileFromImage(name); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"file": name, "deleted": true})
	})

	// POST /api/firmware/sync — copy files from firmwareDir into the mounted image.
	fw.POST("/sync", func(c *gin.Context) {
		if err := ctrl.SyncFirmwareDirToImage(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "synced"})
	})
}

// registerMedia wires the virtual media (ISO) management endpoints.
func registerMedia(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	// GET /api/firmware/media — list staged ISOs and current insertion state.
	fw.GET("/media", func(c *gin.Context) {
		names, err := ctrl.ListMediaFiles()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
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
	fw.POST("/media/upload", func(c *gin.Context) {
		fh, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "multipart field 'file' required"})
			return
		}
		name := filepath.Base(fh.Filename)
		f, err := fh.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer f.Close()
		n, err := ctrl.SaveMediaFile(name, f)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "url required"})
			return
		}
		parsed, err := url.ParseRequestURI(req.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "url must be http or https"})
			return
		}
		name := req.Name
		if name == "" {
			name = filepath.Base(parsed.Path)
		}
		name = filepath.Base(name)
		if name == "." || name == "" || strings.ContainsAny(name, "/\\") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename derived from URL"})
			return
		}
		// #nosec G107 — URL already validated above to http/https only.
		resp, err := http.Get(req.URL) //nolint:noctx
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "fetch failed: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("remote returned %d", resp.StatusCode)})
			return
		}
		n, err := ctrl.SaveMediaFile(name, resp.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
			return
		}
		if err := ctrl.InsertVirtualMedia(req.Name); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, ctrl.GetVirtualMediaState())
	})

	// DELETE /api/firmware/media/:name — remove a staged ISO (must not be inserted).
	fw.DELETE("/media/:name", func(c *gin.Context) {
		name := filepath.Base(c.Param("name"))
		if err := ctrl.DeleteMediaFile(name); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"file": name, "deleted": true})
	})

	// POST /api/firmware/media/eject — eject virtual media and remove vm.iso
	// from firmwareDir and the FAT image.
	fw.POST("/media/eject", func(c *gin.Context) {
		if err := ctrl.EjectVirtualMedia(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, ctrl.GetVirtualMediaState())
	})
}

// registerGadget wires the USB gadget presentation controls.
func registerGadget(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	fw.POST("/present", func(c *gin.Context) {
		if err := ctrl.Present(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "presented"})
	})

	fw.POST("/unpresent", func(c *gin.Context) {
		if err := ctrl.Unpresent(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "unpresented"})
	})
}
