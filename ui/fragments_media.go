package ui

// fragments_media.go serves the navbar virtual-media picker
// (components.VirtualMediaMenu): the "current mount" / "add media" views and
// every mutation (insert existing, upload, fetch by URL, eject). Every
// handler answers with the fragment that should replace #vm-menu-body, so
// the menu always shows what was actually persisted.

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func mediaFragmentRoutes(g *gin.RouterGroup, d *deps.Deps) {
	m := g.Group("/media")

	m.GET("", getMediaMenu(d))
	m.GET("/add", getMediaAdd(d))
	m.POST("/eject", postMediaEject(d))
	m.POST("/insert", postMediaInsert(d))
	m.POST("/upload", postMediaUpload(d))
	m.POST("/fetch", postMediaFetch(d))
	m.GET("/fetch/progress", getMediaFetchProgress(d))
}

// mediaFetchStatus tracks the single in-flight URL fetch, if any: a BMC has
// one admin session at a time, so a package-level tracker (mirroring
// firmware.Controller's IsDownloading sentinel) is enough — no per-request
// state survives the DELIBERATELY DETACHED goroutine otherwise.
type mediaFetchStatus struct {
	Active bool
	Name   string
	Loaded int64
	Total  int64 // 0 until the remote sends a Content-Length.
	Done   bool
	Err    error
}

var (
	mediaFetchMu    sync.Mutex
	mediaFetchState mediaFetchStatus
)

func mediaFetchBusy() bool {
	mediaFetchMu.Lock()
	defer mediaFetchMu.Unlock()
	return mediaFetchState.Active && !mediaFetchState.Done
}

func mediaFetchStart(name string) {
	mediaFetchMu.Lock()
	defer mediaFetchMu.Unlock()
	mediaFetchState = mediaFetchStatus{Active: true, Name: name}
}

func mediaFetchSetTotal(total int64) {
	if total <= 0 {
		return
	}
	mediaFetchMu.Lock()
	mediaFetchState.Total = total
	mediaFetchMu.Unlock()
}

func mediaFetchAddProgress(n int64) {
	mediaFetchMu.Lock()
	mediaFetchState.Loaded += n
	mediaFetchMu.Unlock()
}

func mediaFetchFinish(err error) {
	mediaFetchMu.Lock()
	mediaFetchState.Done = true
	mediaFetchState.Err = err
	mediaFetchMu.Unlock()
}

func mediaFetchSnapshot() mediaFetchStatus {
	mediaFetchMu.Lock()
	defer mediaFetchMu.Unlock()
	return mediaFetchState
}

// countingReader wraps an io.Reader to report every Read into the shared
// fetch tracker, so the progress poller sees bytes as they're copied into
// SaveMediaFile rather than only at completion.
type countingReader struct {
	r      io.Reader
	onRead func(n int64)
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.onRead(int64(n))
	}
	return n, err
}

func getMediaMenu(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		files, inserted := components.MediaState(d.Firmware)
		renderFragment(c, components.VMMenuBody(files, inserted))
	}
}

func getMediaAdd(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		files, _ := components.MediaState(d.Firmware)
		renderFragment(c, components.VMAddBody(files))
	}
}

func postMediaEject(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := d.Firmware.EjectVirtualMedia(); err != nil {
			hxToast(c, "error", "Eject failed", err.Error())
			files, inserted := components.MediaState(d.Firmware)
			renderFragment(c, components.VMMenuBody(files, inserted))
			return
		}
		hxToast(c, "success", "Media ejected", "")
		files, inserted := components.MediaState(d.Firmware)
		renderFragment(c, components.VMMenuBody(files, inserted))
	}
}

func postMediaInsert(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.PostForm("name")
		if name == "" {
			hxToast(c, "error", "Mount failed", "select a file to mount")
			mediaRenderAdd(c, d)
			return
		}
		if err := d.Firmware.InsertVirtualMedia(name); err != nil {
			hxToast(c, "error", "Mount failed", err.Error())
			mediaRenderAdd(c, d)
			return
		}
		hxToast(c, "success", "Mounted "+name, "")
		files, inserted := components.MediaState(d.Firmware)
		renderFragment(c, components.VMMenuBody(files, inserted))
	}
}

func postMediaUpload(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		file, header, err := c.Request.FormFile("file")
		if err != nil {
			hxToast(c, "error", "Upload failed", "no file selected")
			mediaRenderAdd(c, d)
			return
		}
		defer file.Close()

		name := filepath.Base(header.Filename)
		if name == "" || name == "." || name == "/" {
			hxToast(c, "error", "Upload failed", "invalid filename")
			mediaRenderAdd(c, d)
			return
		}
		if _, err := d.Firmware.SaveMediaFile(name, file); err != nil {
			log.Errorf("ui: save media file %q failed: %v", name, err)
			hxToast(c, "error", "Upload failed", err.Error())
			mediaRenderAdd(c, d)
			return
		}
		if err := d.Firmware.InsertVirtualMedia(name); err != nil {
			hxToast(c, "error", "Mount failed", err.Error())
			mediaRenderAdd(c, d)
			return
		}

		hxToast(c, "success", "Uploaded and mounted "+name, "")
		files, inserted := components.MediaState(d.Firmware)
		renderFragment(c, components.VMMenuBody(files, inserted))
	}
}

func postMediaFetch(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawURL := c.PostForm("url")
		name := filepath.Base(c.PostForm("name"))

		parsed, err := url.ParseRequestURI(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			hxToast(c, "error", "Fetch failed", "url must be an http or https URL")
			mediaRenderAdd(c, d)
			return
		}
		if name == "" || name == "." || name == "/" {
			name = filepath.Base(parsed.Path)
		}
		if name == "" || name == "." || name == "/" {
			name = "vm.iso"
		}

		if mediaFetchBusy() {
			hxToast(c, "warning", "Fetch already in progress", "")
			c.Status(http.StatusConflict)
			return
		}
		mediaFetchStart(name)

		// DELIBERATELY DETACHED: the download runs past the request; the
		// poller at GET /media/fetch/progress reports on it until done.
		go func() {
			// #nosec G107 — scheme validated above.
			resp, err := http.Get(rawURL) //nolint:noctx
			if err != nil {
				mediaFetchFinish(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				mediaFetchFinish(fmt.Errorf("remote returned %d", resp.StatusCode))
				return
			}
			mediaFetchSetTotal(resp.ContentLength)

			reader := &countingReader{r: resp.Body, onRead: mediaFetchAddProgress}
			if _, err := d.Firmware.SaveMediaFile(name, reader); err != nil {
				log.Errorf("ui: save fetched media %q failed: %v", name, err)
				mediaFetchFinish(err)
				return
			}
			if err := d.Firmware.InsertVirtualMedia(name); err != nil {
				mediaFetchFinish(err)
				return
			}
			mediaFetchFinish(nil)
		}()

		renderFragment(c, components.VMFetchProgress(name, 0, 0))
	}
}

// getMediaFetchProgress answers the poller embedded in VMFetchProgress:
// while the DELIBERATELY DETACHED download in postMediaFetch is running it
// re-renders the same progress view with fresh byte counts, and once it
// reaches a terminal state it toasts and swaps back to the normal menu.
func getMediaFetchProgress(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		s := mediaFetchSnapshot()
		if !s.Done {
			renderFragment(c, components.VMFetchProgress(s.Name, s.Loaded, s.Total))
			return
		}

		if s.Err != nil {
			hxToast(c, "error", "Fetch failed", s.Err.Error())
			mediaRenderAdd(c, d)
			return
		}
		hxToast(c, "success", "Fetched and mounted "+s.Name, "")
		files, inserted := components.MediaState(d.Firmware)
		renderFragment(c, components.VMMenuBody(files, inserted))
	}
}

// mediaRenderAdd re-renders the add-media view with a fresh file listing —
// the target for every mutation form, success or failure, since a failed
// mount should let the user retry without losing the tab they were on.
func mediaRenderAdd(c *gin.Context, d *deps.Deps) {
	files, _ := components.MediaState(d.Firmware)
	renderFragment(c, components.VMAddBody(files))
}
