package fragments

// fragments_media.go serves the navbar virtual-media picker
// (components.VirtualMediaMenu): the "current mount" / "add media" views and
// every mutation (insert existing, upload, fetch by URL, eject). Every
// handler answers with the fragment that should replace #vm-menu-body, so
// the menu always shows what was actually persisted.

import (
	"io"
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

// maxMediaUploadBytes caps a single ISO push. Uploads stream straight to the
// media directory on the data partition (constant memory), so this exists to
// keep a runaway client from filling that partition, not to bound RAM.
const maxMediaUploadBytes = 8 << 30 // 8 GiB

// mediaFetchTimeout bounds a BMC-initiated ISO download. An 8 GiB image over a
// slow management link is legitimate, so this is generous — but finite, because
// mediaFetchBusy latches on the fetch goroutine and a transfer that never ends
// would wedge the feature until the BMC reboots.
const mediaFetchTimeout = 6 * time.Hour

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

// mediaFetchSetName updates the name shown by the progress poller once
// decompression has sniffed the real format — the name chosen before the
// fetch started (from ?name= or the URL's basename) can't yet reflect a
// compression suffix DecompressingReader hasn't seen bytes for.
func mediaFetchSetName(name string) {
	mediaFetchMu.Lock()
	mediaFetchState.Name = name
	mediaFetchMu.Unlock()
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

// postMediaUpload streams the uploaded ISO straight into the media directory.
//
// It uses utils.StreamMultipartFile rather than c.Request.FormFile on
// purpose: FormFile spools the whole upload into os.TempDir() first, which on
// this device is the RAM-backed tmpfs overlay, so an ISO larger than the
// overlay killed the server partway through the upload. See
// pkg/utils/multipart_stream.go.
func postMediaUpload(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		upload, err := utils.StreamMultipartFile(c.Request, maxMediaUploadBytes, "file")
		if err != nil {
			hxToast(c, "error", "Upload failed", "no file selected")
			mediaRenderAdd(c, d)
			return
		}
		defer upload.Close()

		name := filepath.Base(upload.Filename)
		if name == "" || name == "." || name == "/" {
			hxToast(c, "error", "Upload failed", "invalid filename")
			mediaRenderAdd(c, d)
			return
		}

		// Sniff for gzip/xz/zstd and inflate on the fly; an uncompressed ISO
		// passes through unchanged. maxMediaUploadBytes already bounds the
		// wire via StreamMultipartFile above — LimitDecompressedReader
		// re-bounds the same budget on the inflated side, since a
		// compressed part can expand far past it.
		dr, format, err := utils.DecompressingReader(upload)
		if err != nil {
			hxToast(c, "error", "Upload failed", "decompress failed: "+err.Error())
			mediaRenderAdd(c, d)
			return
		}
		defer dr.Close()
		name = utils.StripCompressionSuffix(name, format)

		if _, err := d.Firmware.SaveMediaFile(name, utils.LimitDecompressedReader(dr, maxMediaUploadBytes)); err != nil {
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
		//
		// utils.FetchURL bounds it. The cap matters because the remote, not
		// the operator, decides how many bytes arrive; the transport timeouts
		// matter more, because mediaFetchBusy latches on this goroutine —
		// a peer that connects and then goes silent would otherwise wedge the
		// fetch feature until the BMC reboots.
		// Detached from the request but bounded by the process context, so a
		// shutdown aborts the transfer rather than being blocked by it.
		ctx, cancel := d.ActionContext(mediaFetchTimeout)
		go func() {
			defer cancel()
			remote, err := utils.FetchURL(ctx, rawURL, maxMediaUploadBytes)
			if err != nil {
				mediaFetchFinish(err)
				return
			}
			defer remote.Close()
			mediaFetchSetTotal(remote.ContentLength)

			// The progress counter wraps the raw wire reader, before
			// decompression, so Loaded stays comparable to Total (the
			// remote's declared Content-Length, which describes the
			// compressed download — not whatever it inflates to).
			counted := &countingReader{r: remote, onRead: mediaFetchAddProgress}

			// Sniff for gzip/xz/zstd and inflate on the fly; an uncompressed
			// ISO passes through unchanged. maxMediaUploadBytes already
			// bounds the wire via FetchURL above — LimitDecompressedReader
			// re-bounds the same budget on the inflated side, since a
			// compressed body can expand far past it.
			dr, format, err := utils.DecompressingReader(counted)
			if err != nil {
				mediaFetchFinish(err)
				return
			}
			defer dr.Close()
			name = utils.StripCompressionSuffix(name, format)
			mediaFetchSetName(name)

			if _, err := d.Firmware.SaveMediaFile(name, utils.LimitDecompressedReader(dr, maxMediaUploadBytes)); err != nil {
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
