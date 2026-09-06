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
	"sync"
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

// A capsule download can take minutes, so the UI needs movement rather than a
// single number at the end. The same callback feeds the Redfish task monitor's
// PercentComplete, which is why the declared total is asserted on every sample
// and not just at the finish.
func TestStageCapsuleFromURLReportsProgress(t *testing.T) {
	body := bytes.Repeat([]byte("A"), 200_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newTestController(t)

	type sample struct{ loaded, total int64 }
	var mu sync.Mutex
	var samples []sample
	err := c.StageCapsuleFromURL(t.Context(), srv.URL+"/host.cap", "host.cap",
		WithProgress(func(loaded, total int64) {
			mu.Lock()
			defer mu.Unlock()
			samples = append(samples, sample{loaded, total})
		}))
	if err != nil {
		t.Fatalf("StageCapsuleFromURL: %v", err)
	}

	if len(samples) < 2 {
		t.Fatalf("got %d progress calls, want movement rather than one final "+
			"number: %+v", len(samples), samples)
	}
	// The first call lands before any bytes, so the UI can show a real total
	// as soon as the headers do.
	if samples[0].loaded != 0 {
		t.Errorf("first sample = %+v, want loaded 0", samples[0])
	}
	for i, s := range samples {
		if s.total != int64(len(body)) {
			t.Errorf("sample %d total = %d, want the declared %d", i, s.total, len(body))
		}
	}
	// loaded must be monotonic and finish on the whole body.
	for i := 1; i < len(samples); i++ {
		if samples[i].loaded < samples[i-1].loaded {
			t.Errorf("loaded went backwards at %d: %d then %d",
				i, samples[i-1].loaded, samples[i].loaded)
		}
	}
	if last := samples[len(samples)-1].loaded; last != int64(len(body)) {
		t.Errorf("final loaded = %d, want the full %d — a bar that stops short "+
			"reads as a stalled transfer", last, len(body))
	}
}

// A chunked response declares no length. Reporting -1 would have every caller
// testing two conditions to decide whether it has a total.
func TestStageCapsuleFromURLReportsZeroTotalWhenUndeclared(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length: flushing forces a chunked response.
		for i := 0; i < 4; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("B"), 4096))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	var totals []int64
	err := newTestController(t).StageCapsuleFromURL(t.Context(), srv.URL+"/x.cap", "x.cap",
		WithProgress(func(_, total int64) { totals = append(totals, total) }))
	if err != nil {
		t.Fatalf("StageCapsuleFromURL: %v", err)
	}
	for i, total := range totals {
		if total != 0 {
			t.Errorf("sample %d total = %d, want 0 for an undeclared length", i, total)
		}
	}
}

// The option is optional. Every existing caller passes none, and that path must
// stay free of the counting wrapper entirely.
func TestStageCapsuleFromURLWorksWithNoProgressOption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("capsule"))
	}))
	defer srv.Close()

	if err := newTestController(t).StageCapsuleFromURL(t.Context(), srv.URL+"/y.cap", "y.cap"); err != nil {
		t.Fatalf("StageCapsuleFromURL with no options: %v", err)
	}
}
