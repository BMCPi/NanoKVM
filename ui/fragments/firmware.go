package fragments

// firmware.go serves the Firmware settings panel: the UEFI FMP capsule volume
// presented to the managed host on lun.0, and what is currently queued on it.
// Mirrors media.go's shape (route group, counting reader for a URL fetch's
// progress, a package-level fetch tracker) applied to capsules instead of
// ISOs.
//
// The controller's own IsStaging()/GetStatus().Staging reflect a sentinel
// file at /tmp/.capsule_staging_in_progress (pkg/firmware/capsule.go) that is
// shared by every process on the box, tests included — see
// firmware_test.go's comment on TestCapsuleFetchRefusesASecondStage. This
// package therefore keeps its own in-memory latch (firmwareFetchState) for
// deciding whether the panel's controls are disabled and whether a second
// concurrent fetch is refused, the same way mediaFetchState does for virtual
// media. The two trackers agree during normal operation (a real fetch holds
// both), but only the local one is what the UI reacts to.

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

// maxCapsuleUploadBytes caps a single capsule upload. A var, not a const:
// firmware_test.go shrinks it to exercise the oversize-rejection path without
// pushing 128 MiB through a test. Uploads stream straight into the capsule
// volume (constant memory), so this bounds what a client can force onto that
// volume, not RAM.
var maxCapsuleUploadBytes int64 = 128 << 20 // 128 MiB

// capsuleFetchTimeout bounds a BMC-initiated capsule download, matching
// api/firmware's capsuleStageTimeout: generous for a slow link to a
// firmware-sized file, but finite so a stalled transfer cannot latch the
// fetch tracker until the next reboot.
const capsuleFetchTimeout = 30 * time.Minute

func firmwareFragmentRoutes(g *gin.RouterGroup, d *deps.Deps) {
	fw := g.Group("/settings/firmware")

	fw.GET("", getFirmwarePanel(d))
	fw.GET("/status", getFirmwareStatus(d))
	fw.POST("/capsules", postFirmwareCapsuleUpload(d))
	fw.POST("/capsules/fetch", postFirmwareCapsuleFetch(d))
	fw.POST("/capsules/clear", postFirmwareCapsuleClear(d))
	fw.DELETE("/capsules/:name", deleteFirmwareCapsule(d))
}

// firmwareFetchStatus tracks the single in-flight URL fetch, if any — see the
// package comment for why this is separate from the controller's own sentinel.
type firmwareFetchStatus struct {
	Active bool
	Name   string
	Done   bool
	Err    error
	// Reported marks that the terminal outcome has already been toasted, so a
	// second poller landing after the first (a request already in flight when
	// the fetch settled) does not raise the toast twice.
	Reported bool
}

var (
	firmwareFetchMu    sync.Mutex
	firmwareFetchState firmwareFetchStatus
)

func firmwareFetchBusy() bool {
	firmwareFetchMu.Lock()
	defer firmwareFetchMu.Unlock()
	return firmwareFetchState.Active && !firmwareFetchState.Done
}

func firmwareFetchStart(name string) {
	firmwareFetchMu.Lock()
	defer firmwareFetchMu.Unlock()
	firmwareFetchState = firmwareFetchStatus{Active: true, Name: name}
}

func firmwareFetchFinish(err error) {
	firmwareFetchMu.Lock()
	defer firmwareFetchMu.Unlock()
	firmwareFetchState.Done = true
	firmwareFetchState.Err = err
}

func firmwareFetchClear() {
	firmwareFetchMu.Lock()
	defer firmwareFetchMu.Unlock()
	firmwareFetchState = firmwareFetchStatus{}
}

func firmwareFetchSnapshot() firmwareFetchStatus {
	firmwareFetchMu.Lock()
	defer firmwareFetchMu.Unlock()
	return firmwareFetchState
}

// firmwareFetchMarkReported flips Reported and reports whether this call is
// the one that found it unset — i.e. whether this caller owns raising the
// outcome toast.
func firmwareFetchMarkReported() bool {
	firmwareFetchMu.Lock()
	defer firmwareFetchMu.Unlock()
	if firmwareFetchState.Reported {
		return false
	}
	firmwareFetchState.Reported = true
	return true
}

// firmwarePanel renders the panel from live controller state plus the fetch
// tracker. Every handler below answers with this, so what an operator sees
// after any action is what was actually persisted, never what was submitted.
func firmwarePanel(d *deps.Deps) components.SettingsFirmware {
	status := d.Firmware.GetStatus()
	snap := firmwareFetchSnapshot()
	return components.SettingsFirmware{
		VolumeReady: status.VolumeReady,
		Presented:   status.Presented,
		VolumeSize:  status.VolumeSize,
		CapsuleDir:  status.CapsuleDir,
		Capsules:    status.Capsules,
		Staging:     snap.Active && !snap.Done,
		StagingName: snap.Name,
	}
}

func getFirmwarePanel(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
	}
}

// getFirmwareStatus answers the poller embedded in the panel while
// m.Staging: while the fetch is still running it just re-renders the same
// (still polling) panel; once it has settled it raises the outcome toast
// exactly once and renders the settled panel, which stops polling because
// firmwarePanel's Staging is now false.
func getFirmwareStatus(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap := firmwareFetchSnapshot()
		if snap.Done && firmwareFetchMarkReported() {
			if snap.Err != nil {
				hxToast(c, "error", "Capsule fetch failed", snap.Err.Error())
			} else {
				hxToast(c, "success", "Staged "+snap.Name, "Applies at the host's next boot.")
			}
		}
		renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
	}
}

// postFirmwareCapsuleUpload streams the uploaded capsule straight onto the
// capsule volume.
//
// It uses utils.StreamMultipartFile rather than c.Request.FormFile /
// ParseMultipartForm on purpose: those spool the whole upload into
// os.TempDir() first, which on this device is the RAM-backed tmpfs overlay.
// See pkg/utils/multipart_stream.go and api/firmware/firmware.go's identical
// handler for the JSON API.
func postFirmwareCapsuleUpload(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		upload, err := utils.StreamMultipartFile(c.Request, maxCapsuleUploadBytes, "file")
		if err != nil {
			hxToast(c, "error", "Upload failed", "no file selected")
			renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
			return
		}
		defer upload.Close()

		name := filepath.Base(upload.Filename)
		if name == "" || name == "." || name == "/" {
			hxToast(c, "error", "Upload failed", "invalid filename")
			renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
			return
		}
		written, err := d.Firmware.StageCapsule(name, upload)
		if err != nil {
			log.Errorf("ui: stage capsule %q failed: %v", name, err)
			hxToast(c, "error", "Upload failed", err.Error())
			renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
			return
		}

		// TransferSummary is called with format "" — capsules are never
		// decompressed (see the package comment), so this only ever reports
		// the size, never extraction language a capsule doesn't earn.
		hxToast(c, "success", "Staged "+name,
			fmt.Sprintf("%s. Applies at the host's next boot.", components.TransferSummary(written, "", 0)))
		renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
	}
}

// postFirmwareCapsuleFetch starts a BMC-initiated capsule download. Rejected
// immediately (no latch taken) for a bad URL or when a fetch is already
// running; otherwise the download runs past this request and the poller at
// GET /settings/firmware/status reports on it until done.
func postFirmwareCapsuleFetch(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawURL := c.PostForm("url")
		name := filepath.Base(c.PostForm("name"))

		parsed, err := url.ParseRequestURI(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			hxToast(c, "error", "Fetch failed", "url must be an http or https URL")
			renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
			return
		}
		if name == "" || name == "." || name == "/" {
			name = filepath.Base(parsed.Path)
		}
		if name == "" || name == "." || name == "/" {
			name = "capsule.cap"
		}

		if firmwareFetchBusy() {
			hxToast(c, "warning", "A capsule is already being staged", "")
			c.Status(http.StatusConflict)
			return
		}
		firmwareFetchStart(name)

		// DELIBERATELY DETACHED: the download outlives this request; the
		// poller at GET /settings/firmware/status reports on it until done.
		// Bounded by the process context, not the request's, so a shutdown
		// aborts the transfer instead of being blocked by it — same hazard
		// postMediaFetch documents.
		ctx, cancel := d.ActionContext(capsuleFetchTimeout)
		go func(rawURL, name string) {
			defer cancel()
			if err := d.Firmware.StageCapsuleFromURL(ctx, rawURL, name); err != nil {
				log.Errorf("ui: capsule fetch failed: %v", err)
				firmwareFetchFinish(err)
				return
			}
			firmwareFetchFinish(nil)
		}(rawURL, name)

		renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
	}
}

func postFirmwareCapsuleClear(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.Firmware.ClearCapsules(); err != nil {
			hxToast(c, "error", "Clear failed", err.Error())
			renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
			return
		}
		hxToast(c, "success", "Queue cleared", "")
		renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
	}
}

func deleteFirmwareCapsule(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := filepath.Base(c.Param("name"))
		if err := d.Firmware.RemoveCapsule(name); err != nil {
			hxToast(c, "error", "Remove failed", err.Error())
			renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
			return
		}
		hxToast(c, "success", "Removed "+name, "")
		renderFragment(c, components.SettingsFirmwareBody(firmwarePanel(d)))
	}
}
