package fragments

// fragments_media_test.go covers the virtual-media upload handler end to end.
//
// The regression it exists for: postMediaUpload used c.Request.FormFile, which
// spools the whole upload into os.TempDir() before the handler sees a byte. On
// this device os.TempDir() is the RAM-backed tmpfs overlay (25% of RAM, see
// the initramfs init in nanokvm-build), so an ISO larger than that overlay
// exhausted RAM partway through the upload and took the server down — the
// browser reported a bare network error at roughly a quarter of the transfer.
//
// The assertion is therefore about *where the bytes go*, not the HTTP status:
// they must land in the media directory on the data partition, with nothing
// written to os.TempDir() at any point.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// mediaRouter mounts the media fragment routes against a Firmware controller
// whose staging directory lives in t's temp dir.
func mediaRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mediaDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Firmware.MediaDir = mediaDir
	d := &deps.Deps{Config: cfg, Firmware: firmware.NewController(cfg, slog.New(slog.DiscardHandler))}

	r := gin.New()
	r.Use(deps.Middleware(d))
	mediaFragmentRoutes(r.Group("/ui"), testHandlers(d))
	return r, mediaDir
}

// testHandlers wraps d in a *handlers with a discard logger, for the
// fragment route-registration functions this package's log-touched files
// (firmware.go, media.go, overview.go, power.go, power_events.go,
// settings.go) register on now rather than on *deps.Deps directly. Shared
// with firmware_test.go and settings_test.go.
func testHandlers(d *deps.Deps) *handlers {
	return &handlers{d: d, log: slog.New(slog.DiscardHandler)}
}

// fakeVMGadget stands in for the configfs-backed USB gadget, absent in a test
// environment. Duplicated from pkg/firmware/virtual_media_test.go's private
// twin rather than shared, since that one isn't exported across packages.
type fakeVMGadget struct{ lun1 string }

func (g *fakeVMGadget) InsertMedia(path string) error { g.lun1 = path; return nil }
func (g *fakeVMGadget) EjectMedia() error             { g.lun1 = ""; return nil }
func (g *fakeVMGadget) LUN1File() (string, bool)      { return g.lun1, g.lun1 != "" }

// mediaRouterWithGadget is mediaRouter plus a fake gadget, so a successful
// upload actually reaches its "Uploaded and mounted" toast instead of
// failing at InsertVirtualMedia the way mediaRouter's callers expect (see
// TestMediaUploadStreamsToDiskWithoutSpooling's comment) — needed here
// because these tests assert on that toast's text, not just where the bytes
// landed.
func mediaRouterWithGadget(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mediaDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Firmware.MediaDir = mediaDir
	ctrl := firmware.NewController(cfg, slog.New(slog.DiscardHandler))
	ctrl.SetVMGadgetForTest(&fakeVMGadget{})
	d := &deps.Deps{Config: cfg, Firmware: ctrl}

	r := gin.New()
	r.Use(deps.Middleware(d))
	mediaFragmentRoutes(r.Group("/ui"), testHandlers(d))
	return r, mediaDir
}

// probingBody reports TMPDIR's contents once, after `at` bytes have been read
// off the wire — i.e. while the upload is genuinely still in flight.
type probingBody struct {
	r     io.Reader
	read  int
	at    int
	fired bool
	probe func()
}

func (p *probingBody) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += n
	if !p.fired && p.read >= p.at {
		p.fired = true
		p.probe()
	}
	return n, err
}

func (p *probingBody) Close() error { return nil }

func tempDirNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestMediaUploadStreamsToDiskWithoutSpooling(t *testing.T) {
	r, mediaDir := mediaRouter(t)

	// TMPDIR isolated from the media dir so "did anything spool?" is
	// unambiguous. os.TempDir() honours $TMPDIR on Unix.
	t.Setenv("TMPDIR", t.TempDir())

	// ~48 MiB: past the 32 MiB gin holds in memory before spooling, so the
	// pre-fix handler is guaranteed to have a temp file open by the probe.
	payload := bytes.Repeat([]byte("nanokvm-virtual-media-"), 2<<20)
	want := sha256.Sum256(payload)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "ubuntu.iso")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	body := buf.Bytes()

	var midFlight []string
	req := httptest.NewRequest(http.MethodPost, "/ui/media/upload", nil)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Body = &probingBody{
		r: bytes.NewReader(body),
		// 90% in: spooling only starts once the 32 MiB memory budget is
		// spent, so an earlier probe would miss it.
		at:    len(body) * 9 / 10,
		probe: func() { midFlight = tempDirNames(t) },
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if midFlight == nil {
		t.Fatal("probe never fired: the request body was not read incrementally")
	}
	if len(midFlight) != 0 {
		t.Errorf("upload spooled into os.TempDir() mid-flight: %v\n"+
			"on the BMC that directory is RAM; the body must stream to the media dir", midFlight)
	}
	if after := tempDirNames(t); len(after) != 0 {
		t.Errorf("upload left files in os.TempDir(): %v", after)
	}

	// The ISO itself must be on the data partition, byte-for-byte.
	saved := filepath.Join(mediaDir, "ubuntu.iso")
	got, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("uploaded ISO not staged in the media dir: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("staged %d bytes, want %d", len(got), len(payload))
	}
	if sha256.Sum256(got) != want {
		t.Error("staged ISO does not match what was uploaded")
	}

	// InsertVirtualMedia needs the USB gadget's configfs, which is absent in
	// a test environment; the handler reports that as a mount failure. The
	// upload half must still have completed, which is what is asserted above.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (handler answers with a fragment either way)", w.Code)
	}
}

func TestMediaUploadRejectsMissingFilePart(t *testing.T) {
	r, mediaDir := mediaRouter(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("notafile", "x"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/ui/media/upload", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if trigger := w.Header().Get("HX-Trigger"); trigger == "" {
		t.Error("want a toast trigger reporting the failure")
	}
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		t.Fatalf("read media dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("media dir should be empty, has %d entries", len(entries))
	}
}

// A path-traversing filename must not escape the media directory.
func TestMediaUploadBasesFilename(t *testing.T) {
	r, mediaDir := mediaRouter(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// multipart.CreateFormFile escapes quotes but not separators, so write
	// the Content-Disposition by hand to get a traversing name on the wire.
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="../../escaped.iso"`}
	fw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := fw.Write([]byte("payload")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/ui/media/upload", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(httptest.NewRecorder(), req)

	if _, err := os.Stat(filepath.Join(filepath.Dir(mediaDir), "escaped.iso")); !os.IsNotExist(err) {
		t.Fatal("upload escaped the media directory")
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "escaped.iso")); err != nil {
		t.Errorf("upload should have been staged under its base name: %v", err)
	}
}

// gzipPayload compresses payload for the extraction-toast tests below.
func gzipPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// The progress bar advancing during a slow xz decode already tells the truth
// about elapsed time (see pkg/streamio/decompress.go); this test is about
// whether the operator learns WHY afterward — the completion toast must name
// both the codec and how much bigger the staged file is than what crossed
// the wire.
func TestMediaUploadCompressedReportsExtractionInToast(t *testing.T) {
	r, mediaDir := mediaRouterWithGadget(t)

	// Kept under 1024 bytes so formatBytes renders it as "N B" — an exact,
	// assertable string — rather than a rounded "X.Y KB".
	payload := bytes.Repeat([]byte("nanokvm-virtual-media-payload-"), 20)
	body, contentType := capsuleUploadBody(t, "ubuntu-24.04.img.gz", gzipPayload(t, payload))

	req := httptest.NewRequest(http.MethodPost, "/ui/media/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	trigger := w.Header().Get("HX-Trigger")
	if !strings.Contains(trigger, "extracted from") || !strings.Contains(trigger, "gzip") {
		t.Errorf("HX-Trigger = %q, want the toast to say the staged size was extracted from a gzip source", trigger)
	}
	if !strings.Contains(trigger, fmt.Sprintf("%d B", len(payload))) {
		t.Errorf("HX-Trigger = %q, want the toast to report the decompressed (staged) size", trigger)
	}

	staged, err := os.ReadFile(filepath.Join(mediaDir, "ubuntu-24.04.img"))
	if err != nil {
		t.Fatalf("decompressed media not staged under its stripped name: %v", err)
	}
	if !bytes.Equal(staged, payload) {
		t.Error("staged content does not match the decompressed payload")
	}
}

// The mirror case: nothing was extracted, so nothing should claim it was —
// only a size, matching what StageCapsule's report looks like for a capsule.
func TestMediaUploadUncompressedReportsSizeOnly(t *testing.T) {
	r, mediaDir := mediaRouterWithGadget(t)

	payload := []byte("a plain image, never touched by any decompressor")
	body, contentType := capsuleUploadBody(t, "plain.iso", payload)

	req := httptest.NewRequest(http.MethodPost, "/ui/media/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	trigger := w.Header().Get("HX-Trigger")
	if strings.Contains(trigger, "extract") {
		t.Errorf("HX-Trigger = %q, an uncompressed upload must not use extraction language", trigger)
	}
	if !strings.Contains(trigger, fmt.Sprintf("%d B", len(payload))) {
		t.Errorf("HX-Trigger = %q, want the toast to report the staged size", trigger)
	}

	if _, err := os.Stat(filepath.Join(mediaDir, "plain.iso")); err != nil {
		t.Fatalf("uncompressed upload not staged: %v", err)
	}
}

// The URL-fetch poller has a live server-side reader to ask, unlike the
// upload form — so once DecompressingReader has sniffed the stream, the
// in-flight fragment and the eventual completion toast must both carry the
// real format, not a placeholder.
func TestMediaFetchStatusCarriesFormatAndReportsExtractionOnCompletion(t *testing.T) {
	r, _ := mediaRouter(t)

	// Latched directly rather than run through a real download — same
	// rationale as TestCapsuleFetchRefusesASecondStage: no network needed to
	// exercise what the poller renders off the tracker's state.
	mediaFetchStart("ubuntu-24.04.img.xz")
	t.Cleanup(mediaFetchClear)

	mediaFetchSetName("ubuntu-24.04.img")
	mediaFetchSetFormat("xz")
	mediaFetchSetTotal(1_100_000_000)
	mediaFetchAddProgress(600_000_000)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/media/fetch/progress", nil))
	// templ HTML-escapes the "&" in the rendered phase text, so match on the
	// parts either side of it rather than hardcoding the escaped entity.
	if body := w.Body.String(); !strings.Contains(body, "Fetching") || !strings.Contains(body, "extracting (xz)") {
		t.Errorf("in-flight fragment does not name the sniffed format; body: %s", body)
	}

	mediaFetchSetWritten(4_200_000_000)
	mediaFetchFinish(nil)

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/media/fetch/progress", nil))
	trigger := w.Header().Get("HX-Trigger")
	if !strings.Contains(trigger, "extracted from") || !strings.Contains(trigger, "xz") {
		t.Errorf("HX-Trigger = %q, want the completion toast to report bytes extracted from an xz source", trigger)
	}
}

// ── delete ──────────────────────────────────────────────────────────────
//
// Deleting a staged image is reachable from the Existing tab's split button:
// pick Delete from the chevron menu, then press the button that replaces
// Mount. The two-step is the confirmation, so the handler below is the only
// thing standing between a click and an unlinked file — which is why the
// mounted-image guard gets its own test rather than being trusted to the UI.

func writeStagedMedia(t *testing.T, mediaDir, name string) string {
	t.Helper()
	path := filepath.Join(mediaDir, name)
	if err := os.WriteFile(path, []byte("not really an iso"), 0o600); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return path
}

func postForm(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMediaDeleteRemovesAStagedFile(t *testing.T) {
	r, mediaDir := mediaRouter(t)
	path := writeStagedMedia(t, mediaDir, "alpine.iso")
	writeStagedMedia(t, mediaDir, "keep-me.iso")

	w := postForm(t, r, "/ui/media/delete", "name=alpine.iso")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — htmx skips the swap on a 4xx/5xx, so the "+
			"refreshed file list would never reach the page", w.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("alpine.iso still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "keep-me.iso")); err != nil {
		t.Errorf("deleting one image took another with it: %v", err)
	}
	if w.Header().Get("HX-Trigger") == "" {
		t.Error("no toast: a destructive action that reports nothing is indistinguishable from a no-op")
	}
	// The response re-renders the Add view rather than jumping back to the
	// mount summary: the operator is managing the library, and the fresh
	// list is what they need to see.
	if body := w.Body.String(); !strings.Contains(body, "keep-me.iso") {
		t.Errorf("response does not carry the refreshed file list:\n%s", body)
	} else if strings.Contains(body, "alpine.iso") {
		t.Errorf("deleted image still listed in the response:\n%s", body)
	}
}

// The controller refuses to unlink the image the gadget is currently serving,
// because the host would see its CD-ROM vanish mid-read. The handler has to
// surface that as a toast rather than swallowing it.
func TestMediaDeleteRefusesTheMountedImage(t *testing.T) {
	r, mediaDir := mediaRouterWithGadget(t)
	path := writeStagedMedia(t, mediaDir, "mounted.iso")

	if w := postForm(t, r, "/ui/media/insert", "name=mounted.iso"); w.Code != http.StatusOK {
		t.Fatalf("setup mount: status %d", w.Code)
	}

	w := postForm(t, r, "/ui/media/delete", "name=mounted.iso")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the mounted image was deleted out from under the gadget: %v", err)
	}
	trigger := w.Header().Get("HX-Trigger")
	if !strings.Contains(trigger, "eject") {
		t.Errorf("HX-Trigger = %q, want a toast telling the operator to eject first", trigger)
	}
}

func TestMediaDeleteRequiresAName(t *testing.T) {
	r, mediaDir := mediaRouter(t)
	writeStagedMedia(t, mediaDir, "alpine.iso")

	w := postForm(t, r, "/ui/media/delete", "name=")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 so the error toast swaps in", w.Code)
	}
	if w.Header().Get("HX-Trigger") == "" {
		t.Error("no toast for an empty selection")
	}
	entries, _ := os.ReadDir(mediaDir)
	if len(entries) != 1 {
		t.Errorf("media dir holds %d entries, want the untouched 1", len(entries))
	}
}

// Path traversal: the name arrives from a form field, so "../../etc/passwd"
// must not escape the media directory. mediaPathFor is the guard; this pins
// that the handler routes through it.
func TestMediaDeleteRefusesATraversingName(t *testing.T) {
	r, mediaDir := mediaRouter(t)
	outside := filepath.Join(filepath.Dir(mediaDir), "outside.iso")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	postForm(t, r, "/ui/media/delete", "name=../outside.iso")

	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a traversing name unlinked a file outside the media dir: %v", err)
	}
}
