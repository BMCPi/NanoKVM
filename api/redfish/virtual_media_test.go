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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

const insertMediaPath = virtualMediaCDPath + "/Actions/VirtualMedia.InsertMedia"

// virtualMediaRouter mounts InsertMedia against a Firmware controller whose
// media directory lives in t's temp dir.
func virtualMediaRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mediaDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Firmware.MediaDir = mediaDir

	svc := NewService(&deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}),
		Firmware: firmware.NewController(cfg),
	})
	r := gin.New()
	r.POST(insertMediaPath, svc.InsertMedia)
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

func staged(t *testing.T, mediaDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		t.Fatalf("read media dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
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
