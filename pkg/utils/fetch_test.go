package utils

// fetch_test.go covers the bounds FetchURL puts on a BMC-initiated download.
// The remote decides how many bytes arrive, so the cap has to hold whether or
// not the remote is honest about Content-Length — and it has to hold without
// buffering the body anywhere, since this device has no RAM to spare.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFetchURLRejectsNonHTTPSchemes(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"ftp://example.com/x.iso",
		"gopher://example.com/x",
		"not a url",
		"",
	} {
		if _, err := FetchURL(t.Context(), raw, 0); err == nil {
			t.Errorf("FetchURL(t.Context(), %q) succeeded; want a scheme rejection", raw)
		}
	}
}

func TestFetchURLRejectsDeclaredOversizeBeforeReading(t *testing.T) {
	// FetchURL never reads the body in this case (that's the point of the
	// test), so the handler goroutine may still be blocked mid-Write, racing
	// the test goroutine's read below, after FetchURL has already returned
	// its error — hence atomic rather than a plain int64.
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(bytes.Repeat([]byte("a"), 4096))
		served.Store(int64(n))
	}))
	defer srv.Close()

	_, err := FetchURL(t.Context(), srv.URL, 1024)
	if !errors.Is(err, ErrRemoteTooLarge) {
		t.Fatalf("err = %v, want ErrRemoteTooLarge", err)
	}
	_ = served.Load() // the point is that the caller never got a reader at all
}

// The cap that actually protects the BMC: a remote that declares nothing (or
// understates) is still bounded, because the limit is enforced while reading.
func TestFetchURLCapsUndeclaredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length: httptest chunks the response.
		for i := 0; i < 64; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("b"), 1024))
		}
	}))
	defer srv.Close()

	remote, err := FetchURL(t.Context(), srv.URL, 4096)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	defer remote.Close()

	n, err := io.Copy(io.Discard, remote)
	if !errors.Is(err, ErrRemoteTooLarge) {
		t.Fatalf("err = %v after %d bytes, want ErrRemoteTooLarge", err, n)
	}
	if n > 4096 {
		t.Errorf("read %d bytes past a 4096 cap", n)
	}
}

func TestFetchURLPassesBodyUnderTheCap(t *testing.T) {
	payload := strings.Repeat("iso", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Declared explicitly: Go only infers Content-Length for responses
		// small enough to sit in its write buffer, and this one is not.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	remote, err := FetchURL(t.Context(), srv.URL, 1<<20)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	defer remote.Close()

	got, err := io.ReadAll(remote)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != payload {
		t.Errorf("body = %d bytes, want %d", len(got), len(payload))
	}
	if remote.ContentLength != int64(len(payload)) {
		t.Errorf("ContentLength = %d, want %d", remote.ContentLength, len(payload))
	}
}

func TestFetchURLUncappedWhenZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("c"), 8192))
	}))
	defer srv.Close()

	remote, err := FetchURL(t.Context(), srv.URL, 0)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	defer remote.Close()

	n, err := io.Copy(io.Discard, remote)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != 8192 {
		t.Errorf("read %d bytes, want 8192", n)
	}
}

func TestFetchURLRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := FetchURL(t.Context(), srv.URL, 0); err == nil {
		t.Fatal("want an error for a 404 response")
	}
}

// A download must never accumulate in os.TempDir(): that is RAM on this board.
func TestFetchURLDoesNotBufferToTempDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 512; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("d"), 64*1024)) // 32 MiB total
		}
	}))
	defer srv.Close()

	remote, err := FetchURL(t.Context(), srv.URL, 0)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	defer remote.Close()

	var mid []string
	read := int64(0)
	buf := make([]byte, 32*1024)
	for {
		n, err := remote.Read(buf)
		read += int64(n)
		if mid == nil && read > 16<<20 {
			entries, _ := os.ReadDir(os.TempDir())
			mid = make([]string, 0, len(entries))
			for _, e := range entries {
				mid = append(mid, e.Name())
			}
		}
		if err != nil {
			break
		}
	}

	if mid == nil {
		t.Fatal("never read far enough to probe")
	}
	if len(mid) != 0 {
		t.Errorf("download buffered into os.TempDir(): %v", mid)
	}
}
