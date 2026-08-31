package streamio

// multipart_stream_test.go pins the property the whole helper exists for: the
// file part is consumed straight off the wire, so nothing is ever spooled into
// os.TempDir(). On the BMC that directory is a RAM-backed tmpfs overlay, and
// spooling there is what killed the server partway through an ISO upload.
//
// The no-spooling assertion is made *while the body is still being read*
// rather than after the handler returns: a check afterwards would also pass on
// a spooling implementation that happened to clean up after itself.

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildMultipart renders a multipart body. Parts are emitted in the order
// given; a part with a non-empty filename becomes a file part.
type part struct {
	name     string
	filename string
	content  []byte
}

func buildMultipart(t *testing.T, parts ...part) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		var (
			fw  io.Writer
			err error
		)
		if p.filename != "" {
			fw, err = w.CreateFormFile(p.name, p.filename)
		} else {
			fw, err = w.CreateFormField(p.name)
		}
		if err != nil {
			t.Fatalf("build multipart part %q: %v", p.name, err)
		}
		if _, err := fw.Write(p.content); err != nil {
			t.Fatalf("write multipart part %q: %v", p.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// watchingBody wraps a request body and runs probe once, after `at` bytes have
// been consumed — i.e. while the upload is genuinely still in flight.
type watchingBody struct {
	r     io.Reader
	read  int
	at    int
	fired bool
	probe func()
}

func (w *watchingBody) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	w.read += n
	if !w.fired && w.read >= w.at {
		w.fired = true
		w.probe()
	}
	return n, err
}

func (w *watchingBody) Close() error { return nil }

// tempDirEntries lists os.TempDir(), which honours $TMPDIR on Unix, so a test
// can isolate it with t.Setenv.
func tempDirEntries(t *testing.T) []string {
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

func TestStreamMultipartFileDoesNotSpoolToTempDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	// Comfortably larger than the 32 MiB gin buffers in memory before it
	// starts spooling, so a ReadForm-based implementation is guaranteed to
	// have a temp file open by the time the probe fires.
	payload := bytes.Repeat([]byte("nanokvm-iso-"), 4<<20) // ~48 MiB
	body, ctype := buildMultipart(t, part{name: "file", filename: "big.iso", content: payload})

	var midFlight []string
	req := httptest.NewRequest(http.MethodPost, "/upload", nil)
	req.Header.Set("Content-Type", ctype)
	req.Body = &watchingBody{
		r: bytes.NewReader(body),
		// 90% in, not 50%: ReadForm holds its first 32 MiB in memory and
		// only then opens the spool file, so a halfway probe would miss it.
		at:    len(body) * 9 / 10,
		probe: func() { midFlight = tempDirEntries(t) },
	}

	upload, err := StreamMultipartFile(req, 0, "file")
	if err != nil {
		t.Fatalf("StreamMultipartFile: %v", err)
	}
	defer upload.Close()

	got, err := io.ReadAll(upload)
	if err != nil {
		t.Fatalf("read upload: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed %d bytes, want %d identical bytes", len(got), len(payload))
	}
	if midFlight == nil {
		t.Fatal("probe never fired: body was not read incrementally")
	}
	if len(midFlight) != 0 {
		t.Errorf("upload spooled to os.TempDir() mid-flight: %v; the body must stream", midFlight)
	}
	if after := tempDirEntries(t); len(after) != 0 {
		t.Errorf("upload left files in os.TempDir(): %v", after)
	}
}

func TestStreamMultipartFileCollectsLeadingAndTrailingValues(t *testing.T) {
	body, ctype := buildMultipart(t,
		part{name: "before", content: []byte("leading")},
		part{name: "Image", filename: "disk.iso", content: []byte("payload")},
		part{name: "after", content: []byte("trailing")},
	)
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)

	upload, err := StreamMultipartFile(req, 0, "Image")
	if err != nil {
		t.Fatalf("StreamMultipartFile: %v", err)
	}
	defer upload.Close()

	if got := upload.Values["before"]; got != "leading" {
		t.Errorf("leading value = %q, want %q", got, "leading")
	}
	// The trailing part is still on the wire until the file is consumed.
	if _, ok := upload.Values["after"]; ok {
		t.Error("trailing value present before the file body was read")
	}

	if _, err := io.Copy(io.Discard, upload); err != nil {
		t.Fatalf("drain file part: %v", err)
	}

	values := upload.Rest()
	if got := values["after"]; got != "trailing" {
		t.Errorf("trailing value = %q, want %q", got, "trailing")
	}
	if got := values["before"]; got != "leading" {
		t.Errorf("leading value lost after Rest(): %q", got)
	}
}

func TestStreamMultipartFileSelectsByFieldName(t *testing.T) {
	body, ctype := buildMultipart(t,
		part{name: "decoy", filename: "wrong.iso", content: []byte("no")},
		part{name: "VirtualMediaImage", filename: "right.iso", content: []byte("yes")},
	)
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)

	upload, err := StreamMultipartFile(req, 0, "Image", "file", "VirtualMediaImage")
	if err != nil {
		t.Fatalf("StreamMultipartFile: %v", err)
	}
	defer upload.Close()

	if upload.Filename != "right.iso" {
		t.Errorf("Filename = %q, want right.iso", upload.Filename)
	}
	got, _ := io.ReadAll(upload)
	if string(got) != "yes" {
		t.Errorf("content = %q, want %q", got, "yes")
	}
}

func TestStreamMultipartFileNoFilePart(t *testing.T) {
	body, ctype := buildMultipart(t, part{name: "onlyavalue", content: []byte("x")})
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)

	_, err := StreamMultipartFile(req, 0, "file")
	if !errors.Is(err, ErrNoFilePart) {
		t.Fatalf("err = %v, want ErrNoFilePart", err)
	}
}

func TestStreamMultipartFileCap(t *testing.T) {
	const limit = 1024

	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"under the cap", limit - 1, false},
		{"exactly at the cap", limit, false},
		{"one byte over", limit + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ctype := buildMultipart(t, part{
				name: "file", filename: "x.iso", content: bytes.Repeat([]byte("a"), tc.size),
			})
			req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
			req.Header.Set("Content-Type", ctype)

			upload, err := StreamMultipartFile(req, limit, "file")
			if err != nil {
				t.Fatalf("StreamMultipartFile: %v", err)
			}
			defer upload.Close()

			n, err := io.Copy(io.Discard, upload)
			switch {
			case tc.wantErr && !errors.Is(err, ErrUploadTooLarge):
				t.Fatalf("err = %v, want ErrUploadTooLarge", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case !tc.wantErr && n != int64(tc.size):
				t.Fatalf("copied %d bytes, want %d", n, tc.size)
			}
		})
	}
}

func TestStreamMultipartFileRejectsOversizedValue(t *testing.T) {
	// An outsized non-file field is dropped rather than buffered: the point
	// of the helper is that nothing unbounded is held in memory.
	huge := strings.Repeat("x", maxMultipartValueBytes+1)
	body, ctype := buildMultipart(t,
		part{name: "meta", content: []byte(huge)},
		part{name: "file", filename: "x.iso", content: []byte("payload")},
	)
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)

	upload, err := StreamMultipartFile(req, 0, "file")
	if err != nil {
		t.Fatalf("StreamMultipartFile: %v", err)
	}
	defer upload.Close()

	if v, ok := upload.Values["meta"]; ok {
		t.Errorf("oversized field buffered (%d bytes); want it dropped", len(v))
	}
	got, _ := io.ReadAll(upload)
	if string(got) != "payload" {
		t.Errorf("content = %q, want %q", got, "payload")
	}
}

func TestStreamMultipartFileRejectsNonMultipart(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")

	if _, err := StreamMultipartFile(req, 0, "file"); err == nil {
		t.Fatal("want error for a non-multipart request")
	}
}

// guard against the helper being handed a path-ish filename; callers are
// expected to base it, and this documents that the helper reports it verbatim.
func TestStreamMultipartFileReportsFilenameVerbatim(t *testing.T) {
	body, ctype := buildMultipart(t, part{
		name: "file", filename: filepath.Join("..", "..", "etc", "passwd"), content: []byte("x"),
	})
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)

	upload, err := StreamMultipartFile(req, 0, "file")
	if err != nil {
		t.Fatalf("StreamMultipartFile: %v", err)
	}
	defer upload.Close()

	// mime/multipart already strips directories from the Content-Disposition
	// filename; the callers base it again regardless.
	if strings.Contains(upload.Filename, "/") {
		t.Errorf("Filename = %q, want no path separators", upload.Filename)
	}
}
