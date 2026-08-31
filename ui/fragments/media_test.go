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
	"crypto/sha256"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	d := &deps.Deps{Config: cfg, Firmware: firmware.NewController(cfg)}

	r := gin.New()
	r.Use(deps.Middleware(d))
	mediaFragmentRoutes(r.Group("/ui"), d)
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
