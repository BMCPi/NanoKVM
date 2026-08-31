package redfish

// virtual_media_test.go covers TransferMethod=Upload, the multipart push a
// Redfish client uses to ship a local ISO to the BMC.
//
// Two properties matter and neither is the HTTP status. First, the image is
// streamed to the media directory rather than spooled into os.TempDir() —
// that directory is a RAM-backed tmpfs overlay on this device, and spooling a
// full ISO through it exhausted RAM partway through the upload. Second, the
// optional InsertMediaRequestBody part names the staged file whether the
// client sends it before or after the image, since streaming means a trailing
// copy only becomes readable once the image is already on disk.

import (
	"bytes"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

const (
	insertMediaPath = virtualMediaCDPath + "/Actions/VirtualMedia.InsertMedia"
	ejectMediaPath  = virtualMediaCDPath + "/Actions/VirtualMedia.EjectMedia"
)

// fakeVMGadget satisfies firmware.VMGadget so insert/eject cycles run without
// the configfs tree, which does not exist in a test environment.
type fakeVMGadget struct{ lun1 string }

func (g *fakeVMGadget) InsertMedia(path string) error { g.lun1 = path; return nil }
func (g *fakeVMGadget) EjectMedia() error             { g.lun1 = ""; return nil }
func (g *fakeVMGadget) LUN1File() (string, bool)      { return g.lun1, g.lun1 != "" }

// virtualMediaRouter mounts the VirtualMedia actions against a Firmware
// controller whose media directory lives in t's temp dir and whose gadget is
// faked, so the full insert/eject lifecycle is exercised.
func virtualMediaRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mediaDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Firmware.MediaDir = mediaDir

	fw := firmware.NewController(cfg, slog.New(slog.DiscardHandler))
	fw.SetVMGadgetForTest(&fakeVMGadget{})

	h := &handlers{
		d: &deps.Deps{
			Power:    power.NewController(config.Hardware{}, config.Power{}, slog.New(slog.DiscardHandler)),
			Firmware: fw,
		},
		log: slog.New(slog.DiscardHandler),
	}
	r := gin.New()
	r.POST(insertMediaPath, h.InsertMedia)
	r.POST(ejectMediaPath, h.EjectMedia)
	return r, mediaDir
}

// uploadPart describes one part of a multipart InsertMedia push.
type uploadPart struct {
	name     string
	filename string // non-empty makes it a file part
	content  string
}

func buildInsertMediaBody(t *testing.T, parts ...uploadPart) (*bytes.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := make(textproto.MIMEHeader)
		if p.filename != "" {
			h.Set("Content-Disposition",
				`form-data; name="`+p.name+`"; filename="`+p.filename+`"`)
			h.Set("Content-Type", "application/octet-stream")
		} else {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"`)
		}
		fw, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("create part %q: %v", p.name, err)
		}
		if _, err := fw.Write([]byte(p.content)); err != nil {
			t.Fatalf("write part %q: %v", p.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return bytes.NewReader(buf.Bytes()), w.FormDataContentType()
}

// staged lists the media files in mediaDir, excluding dotfiles — those are
// lifecycle bookkeeping (ephemeral markers), not staged images.
func staged(t *testing.T, mediaDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		t.Fatalf("read media dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

func TestInsertMediaUploadStagesFilePart(t *testing.T) {
	r, mediaDir := virtualMediaRouter(t)

	body, ctype := buildInsertMediaBody(t, uploadPart{
		name: "Image", filename: "alpine.iso", content: "ISO-BYTES",
	})
	req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
	req.Header.Set("Content-Type", ctype)
	r.ServeHTTP(httptest.NewRecorder(), req)

	got, err := os.ReadFile(filepath.Join(mediaDir, "alpine.iso"))
	if err != nil {
		t.Fatalf("image not staged: %v (dir has %v)", err, staged(t, mediaDir))
	}
	if string(got) != "ISO-BYTES" {
		t.Errorf("staged content = %q, want %q", got, "ISO-BYTES")
	}
}

// InsertMediaRequestBody may arrive before or after the image. Streaming means
// only the leading form can be honoured up front, so the trailing form has to
// be reconciled after the file lands — both must end at the same filename.
func TestInsertMediaUploadHonoursRequestBodyEitherSide(t *testing.T) {
	const meta = `{"Image":"named-by-client.iso","TransferMethod":"Upload"}`

	for _, tc := range []struct {
		name  string
		parts []uploadPart
	}{
		{"metadata before the image", []uploadPart{
			{name: "InsertMediaRequestBody", content: meta},
			{name: "Image", filename: "wire-name.iso", content: "ISO"},
		}},
		{"metadata after the image", []uploadPart{
			{name: "Image", filename: "wire-name.iso", content: "ISO"},
			{name: "InsertMediaRequestBody", content: meta},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, mediaDir := virtualMediaRouter(t)

			body, ctype := buildInsertMediaBody(t, tc.parts...)
			req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
			req.Header.Set("Content-Type", ctype)
			r.ServeHTTP(httptest.NewRecorder(), req)

			if _, err := os.Stat(filepath.Join(mediaDir, "named-by-client.iso")); err != nil {
				t.Fatalf("want the client's Image name staged, dir has %v", staged(t, mediaDir))
			}
			if names := staged(t, mediaDir); len(names) != 1 {
				t.Errorf("media dir has %v, want exactly the renamed image", names)
			}
		})
	}
}

func TestInsertMediaUploadAcceptsAlternateFieldNames(t *testing.T) {
	// redfishtool sends "file"; some tools use the resource name.
	for _, field := range []string{"Image", "file", "VirtualMediaImage"} {
		t.Run(field, func(t *testing.T) {
			r, mediaDir := virtualMediaRouter(t)

			body, ctype := buildInsertMediaBody(t, uploadPart{
				name: field, filename: "x.iso", content: "ISO",
			})
			req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
			req.Header.Set("Content-Type", ctype)
			r.ServeHTTP(httptest.NewRecorder(), req)

			if _, err := os.Stat(filepath.Join(mediaDir, "x.iso")); err != nil {
				t.Errorf("field %q not accepted: dir has %v", field, staged(t, mediaDir))
			}
		})
	}
}

func TestInsertMediaUploadRejectsContradictoryTransferMethod(t *testing.T) {
	r, mediaDir := virtualMediaRouter(t)

	body, ctype := buildInsertMediaBody(t,
		uploadPart{name: "InsertMediaRequestBody", content: `{"TransferMethod":"Stream"}`},
		uploadPart{name: "Image", filename: "x.iso", content: "ISO"},
	)
	req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if names := staged(t, mediaDir); len(names) != 0 {
		t.Errorf("nothing should have been staged, dir has %v", names)
	}
}

func TestInsertMediaUploadWithoutFilePart(t *testing.T) {
	r, _ := virtualMediaRouter(t)

	body, ctype := buildInsertMediaBody(t,
		uploadPart{name: "InsertMediaRequestBody", content: `{"Image":"x.iso"}`},
	)
	req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// Redfish media is transient: the client that inserted it has no delete verb,
// so ejecting is its last reference to the image — the staged file goes with
// the eject instead of accumulating on the data partition.
func TestInsertMediaUploadIsEphemeralAcrossEject(t *testing.T) {
	r, mediaDir := virtualMediaRouter(t)

	body, ctype := buildInsertMediaBody(t, uploadPart{
		name: "Image", filename: "transient.iso", content: "ISO",
	})
	req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("insert status = %d, want 200", w.Code)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "transient.iso")); err != nil {
		t.Fatalf("image not staged while mounted: %v", err)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ejectMediaPath, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("eject status = %d, want 204", w.Code)
	}

	if names := staged(t, mediaDir); len(names) != 0 {
		t.Errorf("media dir has %v after eject; a Redfish insert must not outlive its mount", names)
	}
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		t.Fatalf("read media dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("bookkeeping left behind after eject: %v", entries)
	}
}

// The Stream transfer method (BMC pulls the image from a URL) carries the same
// transient lifecycle as an upload.
func TestInsertMediaStreamIsEphemeralAcrossEject(t *testing.T) {
	r, mediaDir := virtualMediaRouter(t)

	iso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("REMOTE-ISO-BYTES"))
	}))
	defer iso.Close()

	body := strings.NewReader(`{"Image":"` + iso.URL + `/remote.iso"}`)
	req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("insert status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(mediaDir, "remote.iso"))
	if err != nil {
		t.Fatalf("fetched image not staged: %v (dir has %v)", err, staged(t, mediaDir))
	}
	if string(got) != "REMOTE-ISO-BYTES" {
		t.Errorf("staged content = %q, want the remote body", got)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ejectMediaPath, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("eject status = %d, want 204", w.Code)
	}
	if names := staged(t, mediaDir); len(names) != 0 {
		t.Errorf("media dir has %v after eject; a streamed insert must not outlive its mount", names)
	}
}

// An insert that stages bytes but fails to mount must not strand them: the
// client has no way to delete the orphan.
func TestInsertMediaFailedMountCleansUpStagedFile(t *testing.T) {
	r, mediaDir := virtualMediaRouter(t)

	first, ctype := buildInsertMediaBody(t, uploadPart{
		name: "Image", filename: "mounted.iso", content: "ISO",
	})
	req := httptest.NewRequest(http.MethodPost, insertMediaPath, first)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first insert status = %d, want 200", w.Code)
	}

	// Second insert while the first is mounted: staged, then refused.
	second, ctype := buildInsertMediaBody(t, uploadPart{
		name: "Image", filename: "refused.iso", content: "ISO",
	})
	req = httptest.NewRequest(http.MethodPost, insertMediaPath, second)
	req.Header.Set("Content-Type", ctype)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("second insert status = %d, want 409", w.Code)
	}

	names := staged(t, mediaDir)
	if len(names) != 1 || names[0] != "mounted.iso" {
		t.Errorf("media dir has %v, want only the mounted image; the refused upload must be cleaned up", names)
	}
}

// A large push must never touch os.TempDir(): that is the RAM-backed overlay
// on the BMC, and spooling an ISO through it is what crashed the server.
func TestInsertMediaUploadDoesNotSpool(t *testing.T) {
	r, mediaDir := virtualMediaRouter(t)
	t.Setenv("TMPDIR", t.TempDir())

	// Past the 32 MiB a ReadForm-based handler holds in memory before it
	// starts writing a spool file.
	payload := bytes.Repeat([]byte("virtual-media-payload-"), 2<<20) // ~46 MiB
	body, ctype := buildInsertMediaBody(t, uploadPart{
		name: "Image", filename: "big.iso", content: string(payload),
	})
	req := httptest.NewRequest(http.MethodPost, insertMediaPath, body)
	req.Header.Set("Content-Type", ctype)
	r.ServeHTTP(httptest.NewRecorder(), req)

	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("upload spooled into os.TempDir(): %v; the image must stream to disk", names)
	}

	info, err := os.Stat(filepath.Join(mediaDir, "big.iso"))
	if err != nil {
		t.Fatalf("image not staged: %v (dir has %v)", err, staged(t, mediaDir))
	}
	if info.Size() != int64(len(payload)) {
		t.Errorf("staged %d bytes, want %d", info.Size(), len(payload))
	}
}
