// Package firmware exposes the firmware API: image status and download,
// bootloader EEPROM staging, direct FAT file management, virtual media,
// USB gadget presentation, and BIOS (U-Boot) version management.
package firmware

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// Upload body caps. Uploads are streamed to the image (constant memory), so
// these bound the on-image/disk size against abuse rather than RAM.
const (
	maxFileUploadBytes   = 128 << 20 // 128 MiB — boot-partition files
	maxEEPROMConfigBytes = 1 << 20   // 1 MiB — bootconf text is tiny
)

// Register mounts the firmware routes on the shared authenticated group.
func Register(api *gin.RouterGroup) {
	ctrl := firmware.GetController()
	fw := api.Group("/firmware")

	registerImage(fw, ctrl)
	registerEEPROM(fw, ctrl)
	registerFiles(fw, ctrl)
	registerMedia(fw, ctrl)
	registerGadget(fw, ctrl)
	registerBIOS(fw, ctrl)
}

// registerImage wires image status/download plus the U-Boot env, inventory
// and boot-target views.
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

	fw.GET("/env", func(c *gin.Context) {
		vars, err := ctrl.GetAllEnvVars()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, vars)
	})

	fw.GET("/inventory", func(c *gin.Context) {
		inv, err := ctrl.GetInventory()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, inv)
	})

	fw.GET("/boot", func(c *gin.Context) {
		bt, err := ctrl.GetBootTargets()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"persistent": bt.Persistent,
			"once":       bt.Once,
			"effective":  bt.Effective,
		})
	})

	fw.PATCH("/boot", func(c *gin.Context) {
		var req struct {
			BootTargets string `json:"boot_targets"`
			Persistence string `json:"persistence"` // "once" (default) or "continuous"
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		var setErr error
		if req.Persistence == "continuous" {
			setErr = ctrl.SetBootTarget(req.BootTargets)
		} else {
			setErr = ctrl.SetBootTargetOnce(req.BootTargets)
		}
		if setErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": setErr.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"boot_targets": req.BootTargets, "persistence": req.Persistence})
	})
}

// registerEEPROM wires the RPi bootloader EEPROM config endpoints.
func registerEEPROM(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	// GET /api/firmware/eeprom — returns the current bootloader config:
	// raw text + section-grouped parse + provenance (which file it came
	// from) + Pending flag when a staged pieeprom.upd is present.
	fw.GET("/eeprom", func(c *gin.Context) {
		summary, err := ctrl.GetEEPROMConfig()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, summary)
	})

	// PUT /api/firmware/eeprom — stages a new bootconf.txt as pieeprom.upd
	// using the rpieeprom binary-image updater. Body is either
	// application/json `{"content":"..."}` or raw text/plain. The host's
	// rpi-eeprom-update flashes pieeprom.upd on the next boot.
	fw.PUT("/eeprom", func(c *gin.Context) {
		var content string
		ct := c.GetHeader("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			var body struct {
				Content string `json:"content"`
			}
			if err := c.ShouldBindJSON(&body); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			content = body.Content
		} else {
			raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxEEPROMConfigBytes))
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			content = string(raw)
		}
		summary, err := ctrl.SetEEPROMConfig(c.Request.Context(), content)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, summary)
	})

	// DELETE /api/firmware/eeprom/pending — cancels a staged update by
	// removing pieeprom.upd. Next read shows the live pieeprom.bin config
	// with Pending=false.
	fw.DELETE("/eeprom/pending", func(c *gin.Context) {
		if err := ctrl.CancelEEPROMUpdate(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	})

	// GET /api/firmware/eeprom/latest — peek at the latest upstream
	// pieeprom-*.bin metadata (name/version/size/url) without committing
	// to a download. Useful for showing "new version available" badges.
	fw.GET("/eeprom/latest", func(c *gin.Context) {
		img, err := ctrl.LatestPieepromImage(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, img)
	})

	// POST /api/firmware/eeprom/recovery/refresh — force a re-download
	// of recovery.bin from the upstream channel, overwriting whatever's
	// on the FAT. Use after the bootloader release changes so the
	// recovery loader on disk matches.
	//
	// pieeprom.bin is intentionally NOT re-downloaded by the BMC — it's
	// the live EEPROM dump U-Boot writes each boot and serves as the
	// recovery source for the host.
	fw.POST("/eeprom/recovery/refresh", func(c *gin.Context) {
		if err := ctrl.RefreshRecoveryBin(c.Request.Context()); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		summary, _ := ctrl.GetEEPROMConfig()
		c.JSON(http.StatusOK, summary)
	})

	// POST /api/firmware/eeprom/upgrade — stage a bootloader version
	// upgrade by downloading the latest upstream pieeprom-*.bin,
	// transplanting the user's current bootconf into it, and writing it
	// as pieeprom.upd. recovery.bin from the same channel is staged in
	// the same step. The live pieeprom.bin is untouched.
	fw.POST("/eeprom/upgrade", func(c *gin.Context) {
		summary, err := ctrl.StageEEPROMVersionUpgrade(c.Request.Context())
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, firmware.ErrNoPieepromBin) {
				status = http.StatusConflict
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, summary)
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

// registerBIOS wires the BIOS (U-Boot) version/update endpoints and the
// kernel-version → U-Boot image management.
func registerBIOS(fw *gin.RouterGroup, ctrl *firmware.Controller) {
	// GET /api/firmware/bios/version — current installed BIOS (u-boot) version
	// (from machine.env's `ver` variable) and the latest release available.
	fw.GET("/bios/version", func(c *gin.Context) {
		info, err := ctrl.GetUBootVersionInfo()
		if err != nil {
			// Return what we have (current may still be filled in) plus the error.
			c.JSON(http.StatusOK, gin.H{
				"current":         info.Current,
				"latest":          info.Latest,
				"updateAvailable": info.UpdateAvailable,
				"error":           err.Error(),
			})
			return
		}
		c.JSON(http.StatusOK, info)
	})

	// POST /api/firmware/bios/update — download the latest BIOS (u-boot) image
	// (preserving env files). Optional body: { "url": "..." } overrides
	// the latest-release lookup.
	fw.POST("/bios/update", func(c *gin.Context) {
		if ctrl.IsDownloading() {
			c.JSON(http.StatusConflict, gin.H{"error": "download already in progress"})
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		_ = c.ShouldBindJSON(&req) // body is optional

		go func(url string) {
			var err error
			if url != "" {
				err = ctrl.UpdateUBootFromURL(url)
			} else {
				err = ctrl.UpdateUBoot()
			}
			if err != nil {
				log.Errorf("u-boot update failed: %v", err)
			}
		}(req.URL)

		c.JSON(http.StatusAccepted, gin.H{"message": "update started"})
	})

	// GET /api/firmware/bios/kernels — list all supported kernel versions with
	// their mapped U-Boot version and local download/active state.
	fw.GET("/bios/kernels", func(c *gin.Context) {
		// Prefer the explicit activation-tracking file: machine.env still holds
		// the OLD ver string after activation until the board reboots, so
		// GetUBootVersionInfo().Current would return the wrong value.
		activeVer := ctrl.ActiveUBootVersion()
		if activeVer == "" {
			// No versioned activation recorded — fall back to reading machine.env.
			if info, err := ctrl.GetUBootVersionInfo(); err == nil {
				activeVer = info.Current
			}
		}

		kernels := make([]gin.H, 0, len(firmware.KernelUBootMap))
		for _, k := range firmware.KernelVersionsSorted() {
			ubootVer := firmware.KernelUBootMap[k]
			downloaded := ctrl.VersionedImageExists(ubootVer)
			active := activeVer != "" &&
				strings.EqualFold(
					strings.TrimPrefix(activeVer, "v"),
					strings.TrimPrefix(ubootVer, "v"),
				)
			kernels = append(kernels, gin.H{
				"kernel":     k,
				"uboot":      ubootVer,
				"downloaded": downloaded,
				"active":     active,
			})
		}
		c.JSON(http.StatusOK, gin.H{"kernels": kernels})
	})

	// POST /api/firmware/bios/kernel/:kernel/download — download and cache the
	// U-Boot image for the given kernel version without activating it.
	// Optional query: ?force=true deletes any existing cached image first,
	// forcing a fresh download even if the file is already present. When
	// force=true AND the kernel's U-Boot version is the currently-active one,
	// the freshly-downloaded image is automatically swapped into the active
	// slot (preserving env files) — "refresh" otherwise leaves the active
	// boot image untouched, which is surprising when the user is refreshing
	// the version they're already running.
	fw.POST("/bios/kernel/:kernel/download", func(c *gin.Context) {
		kernel := c.Param("kernel")
		ubootVer, ok := firmware.KernelUBootMap[kernel]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown kernel version %q", kernel)})
			return
		}
		if ctrl.IsDownloading() {
			c.JSON(http.StatusConflict, gin.H{"error": "download already in progress"})
			return
		}
		rel, err := firmware.ReleaseByVersion(ubootVer)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		force := c.Query("force") == "true"
		// Snapshot active version BEFORE the download so the auto-reactivate
		// decision isn't affected by a concurrent activation racing this one.
		wasActive := force && ctrl.ActiveUBootVersion() == ubootVer
		go func(ver, url string, force, reactivate bool) {
			if force {
				ctrl.DeleteVersionedImage(ver)
			}
			if err := ctrl.DownloadVersionedImage(ver, url); err != nil {
				log.Errorf("versioned image download failed (%s): %v", ver, err)
				return
			}
			if reactivate {
				if err := ctrl.ActivateVersionedImage(ver); err != nil {
					log.Errorf("auto-reactivate after refresh failed (%s): %v", ver, err)
				} else {
					log.Infof("auto-reactivated %s after refresh of currently-active image", ver)
				}
			}
		}(ubootVer, rel.AssetURL, force, wasActive)
		c.JSON(http.StatusAccepted, gin.H{
			"message":      "download started",
			"uboot":        ubootVer,
			"reactivating": wasActive,
		})
	})

	// POST /api/firmware/bios/kernel/:kernel/activate — swap the cached image
	// for the given kernel version into the active slot, preserving env files.
	fw.POST("/bios/kernel/:kernel/activate", func(c *gin.Context) {
		kernel := c.Param("kernel")
		ubootVer, ok := firmware.KernelUBootMap[kernel]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown kernel version %q", kernel)})
			return
		}
		if err := ctrl.ActivateVersionedImage(ubootVer); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "activated", "uboot": ubootVer})
	})
}
