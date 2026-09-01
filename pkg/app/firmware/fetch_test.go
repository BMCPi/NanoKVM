package firmware

// fetch_test.go pins where a capsule download is staged.
//
// StageCapsuleFromURL used os.CreateTemp(""), which puts the staging file in
// os.TempDir(). On this device that is the tmpfs overlay over the squashfs
// root — a few tens of megabytes of RAM — so downloading a capsule filled the
// overlay and took the server down mid-transfer. The staging file has to land
// beside the capsule volume on the data partition instead.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func tempDirListing(t *testing.T) []string {
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

func TestStageCapsuleFromURLDoesNotUseTempDir(t *testing.T) {
	// os.TempDir() honours $TMPDIR on Unix, so this isolates the assertion.
	t.Setenv("TMPDIR", t.TempDir())

	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}

	// Big enough that a staging file in a small tmpfs would be the problem
	// this test exists to catch, small enough to stay quick.
	payload := bytes.Repeat([]byte("CAPSULE!"), 512*1024) // 4 MiB

	var probed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		half := len(payload) / 2
		_, _ = w.Write(payload[:half])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Mid-transfer: whatever the staging file is, it is open by now.
		probed = tempDirListing(t)
		_, _ = w.Write(payload[half:])
	}))
	defer srv.Close()

	if err := c.StageCapsuleFromURL(t.Context(), srv.URL+"/host.cap", "host.cap"); err != nil {
		t.Fatalf("StageCapsuleFromURL: %v", err)
	}

	if probed == nil {
		t.Fatal("probe never ran: the handler did not stream")
	}
	if len(probed) != 0 {
		t.Errorf("capsule staged through os.TempDir(): %v\n"+
			"that directory is RAM on this device; stage on the data partition", probed)
	}
	if after := tempDirListing(t); len(after) != 0 {
		t.Errorf("capsule fetch left files in os.TempDir(): %v", after)
	}

	// And it must have actually landed in the capsule volume.
	capsules, err := c.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	var found bool
	for _, staged := range capsules {
		if strings.EqualFold(filepath.Base(staged.Name), "host.cap") {
			found = true
		}
	}
	if !found {
		t.Errorf("capsule not staged into the volume; got %v", capsules)
	}
}

// WithProgress feeds the Redfish task monitor's PercentComplete: the callback
// must see the declared total and a byte count that reaches it.
func TestStageCapsuleFromURLReportsProgress(t *testing.T) {
	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}

	payload := bytes.Repeat([]byte("PROGRESS"), 128*1024) // 1 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Declare the length the way a real file server does — otherwise a
		// body past Go's write buffer goes chunked and progress has no total.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	var lastLoaded, lastTotal int64
	var calls int
	err := c.StageCapsuleFromURL(t.Context(), srv.URL+"/p.cap", "p.cap",
		WithProgress(func(loaded, total int64) {
			if loaded < lastLoaded {
				t.Errorf("progress went backwards: %d after %d", loaded, lastLoaded)
			}
			lastLoaded, lastTotal = loaded, total
			calls++
		}))
	if err != nil {
		t.Fatalf("StageCapsuleFromURL: %v", err)
	}
	if calls == 0 {
		t.Fatal("progress callback never ran")
	}
	if lastLoaded != int64(len(payload)) {
		t.Errorf("final loaded = %d, want %d", lastLoaded, len(payload))
	}
	if lastTotal != int64(len(payload)) {
		t.Errorf("reported total = %d, want the declared Content-Length %d", lastTotal, len(payload))
	}
}

func TestStageCapsuleFromURLRejectsOversize(t *testing.T) {
	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Declare more than the capsule cap allows; the fetch must refuse
		// before writing anything to the data partition.
		w.Header().Set("Content-Length", "1099511627776") // 1 TiB
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := c.StageCapsuleFromURL(t.Context(), srv.URL+"/huge.cap", "huge.cap")
	if err == nil {
		t.Fatal("want an error for an oversized capsule")
	}
}

func TestStageCapsuleFromURLRejectsBadScheme(t *testing.T) {
	c := newTestController(t)
	if err := c.StageCapsuleFromURL(t.Context(), "file:///etc/passwd", "x.cap"); err == nil {
		t.Fatal("want an error for a non-http(s) URL")
	}
}
