package utils

// decompress_test.go pins the properties that make DecompressingReader safe
// to drop into three existing upload/fetch call sites without re-auditing
// each one: every supported format round-trips exactly, unrecognised input
// is untouched byte-for-byte, corruption is reported rather than silently
// truncated, a decompression bomb is capped rather than exhausting storage,
// and nothing along the way spools through the RAM-backed os.TempDir().

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func gzipBytes(t *testing.T, payload []byte) []byte {
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

func xzBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("xz write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("xz close: %v", err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func TestDecompressingReaderRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("nanokvm virtual media round trip "), 4096)

	cases := []struct {
		name       string
		compressed []byte
		wantFormat string
	}{
		{"gzip", gzipBytes(t, payload), "gzip"},
		{"xz", xzBytes(t, payload), "xz"},
		{"zstd", zstdBytes(t, payload), "zstd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, format, err := DecompressingReader(bytes.NewReader(tc.compressed))
			if err != nil {
				t.Fatalf("DecompressingReader: %v", err)
			}
			defer rc.Close()

			if format != tc.wantFormat {
				t.Errorf("format = %q, want %q", format, tc.wantFormat)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
			}
		})
	}
}

// The safety property: input that doesn't match any recognised header must
// come back exactly as it went in. This is what lets DecompressingReader sit
// ahead of a raw .iso/.img without changing that path's behaviour at all.
func TestDecompressingReaderPassthroughIsByteIdentical(t *testing.T) {
	cases := map[string][]byte{
		"plain image bytes":                bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 8192),
		"empty":                            {},
		"shorter than any magic (3 bytes)": {0x00, 0x01, 0x02},
		"exactly one byte":                 {0x7f},
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			rc, format, err := DecompressingReader(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("DecompressingReader: %v", err)
			}
			defer rc.Close()

			if format != "" {
				t.Errorf("format = %q, want \"\" (no header should match)", format)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("passthrough mismatch: got %d bytes, want %d", len(got), len(payload))
			}
		})
	}
}

// A stream that merely starts with plausible-looking bytes but isn't
// actually gzip must fail cleanly rather than being mistaken for a match by
// some looser check than a full prefix comparison.
func TestDecompressingReaderRejectsCorruptGzipHeader(t *testing.T) {
	corrupt := append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte{0xff}, 64)...)
	_, _, err := DecompressingReader(bytes.NewReader(corrupt))
	if err == nil {
		t.Fatal("want an error for a corrupt gzip header, got nil")
	}
}

// A stream cut off mid-body (as a dropped upload connection would produce)
// must surface as an error from Read, not as a quiet short read that looks
// like a successful, truncated file.
func TestDecompressingReaderTruncatedStreamErrors(t *testing.T) {
	payload := bytes.Repeat([]byte("truncate me "), 4096)

	cases := map[string][]byte{
		"gzip": gzipBytes(t, payload),
		"xz":   xzBytes(t, payload),
		"zstd": zstdBytes(t, payload),
	}

	for name, compressed := range cases {
		t.Run(name, func(t *testing.T) {
			truncated := compressed[:len(compressed)/2]
			rc, _, err := DecompressingReader(bytes.NewReader(truncated))
			if err != nil {
				// A truncation landing inside the header itself is also an
				// acceptable failure mode.
				return
			}
			defer rc.Close()

			_, err = io.ReadAll(rc)
			if err == nil {
				t.Fatal("want an error reading a truncated stream, got nil")
			}
		})
	}
}

// The decompression-bomb defense: a small, highly compressible input must
// not be allowed to inflate past the caller's cap.
func TestLimitDecompressedReaderRejectsBomb(t *testing.T) {
	const capBytes = 64 * 1024                       // 64 KiB
	bomb := gzipBytes(t, make([]byte, 16*1024*1024)) // 16 MiB of zeros

	rc, format, err := DecompressingReader(bytes.NewReader(bomb))
	if err != nil {
		t.Fatalf("DecompressingReader: %v", err)
	}
	defer rc.Close()
	if format != "gzip" {
		t.Fatalf("format = %q, want gzip", format)
	}

	limited := LimitDecompressedReader(rc, capBytes)
	n, err := io.Copy(io.Discard, limited)
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("err = %v, want ErrDecompressedTooLarge", err)
	}
	if n > capBytes {
		t.Errorf("read %d bytes past a %d cap", n, capBytes)
	}
}

func TestLimitDecompressedReaderPassesBodyUnderCap(t *testing.T) {
	payload := []byte("well within the cap")
	got, err := io.ReadAll(LimitDecompressedReader(bytes.NewReader(payload), 1<<20))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestLimitDecompressedReaderUncappedWhenZero(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 8192)
	got, err := io.ReadAll(LimitDecompressedReader(bytes.NewReader(payload), 0))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("read %d bytes, want %d", len(got), len(payload))
	}
}

func TestStripCompressionSuffix(t *testing.T) {
	cases := []struct {
		name, format, want string
	}{
		{"ubuntu-24.04.img.xz", "xz", "ubuntu-24.04.img"},
		{"blob.gz", "gzip", "blob"},
		{"blob.gzip", "gzip", "blob"},
		{"image.zst", "zstd", "image"},
		{"image.zstd", "zstd", "image"},
		{"ubuntu.img", "", "ubuntu.img"},           // no format detected: untouched
		{"ubuntu.img.xz", "", "ubuntu.img.xz"},     // format not detected even though name looks compressed
		{"ubuntu.img.xz", "gzip", "ubuntu.img.xz"}, // wrong format for the suffix present: untouched
		{".xz", "xz", ".xz"},                       // suffix is the whole name: nothing sensible to strip
	}

	for _, tc := range cases {
		if got := StripCompressionSuffix(tc.name, tc.format); got != tc.want {
			t.Errorf("StripCompressionSuffix(%q, %q) = %q, want %q", tc.name, tc.format, got, tc.want)
		}
	}
}

// No step of sniffing or decoding may spool through os.TempDir(): on this
// board that directory is a 30 MB RAM overlay, and a multi-gigabyte image is
// exactly what this feature exists to accept. Mirrors
// TestFetchURLDoesNotBufferToTempDir in fetch_test.go.
func TestDecompressingReaderDoesNotBufferToTempDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		zw := gzip.NewWriter(w)
		defer zw.Close()
		chunk := bytes.Repeat([]byte("e"), 64*1024)
		for i := 0; i < 512; i++ { // 32 MiB decompressed (gzip's flate makes this compress poorly, which is fine: the point is bytes in flight, not ratio)
			_, _ = zw.Write(chunk)
		}
	}))
	defer srv.Close()

	remote, err := FetchURL(t.Context(), srv.URL, 0)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	defer remote.Close()

	rc, format, err := DecompressingReader(remote)
	if err != nil {
		t.Fatalf("DecompressingReader: %v", err)
	}
	defer rc.Close()
	if format != "gzip" {
		t.Fatalf("format = %q, want gzip", format)
	}

	var mid []string
	read := int64(0)
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
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
		t.Errorf("decompression buffered into os.TempDir(): %v", mid)
	}
}

// Guard against a magic-byte coincidence: plain data that happens to start
// with a real gzip header but isn't gzip-formatted after all is exactly the
// corrupt-header case above; this test instead pins that our magic slices
// are compared as full prefixes, not first-byte-only.
func TestMagicPrefixComparisonIsFullNotPartial(t *testing.T) {
	// Starts like gzip's first byte but diverges on the second.
	notGzip := append([]byte{0x1f, 0x00}, strings.Repeat("x", 32)...)
	rc, format, err := DecompressingReader(bytes.NewReader(notGzip))
	if err != nil {
		t.Fatalf("DecompressingReader: %v", err)
	}
	defer rc.Close()
	if format != "" {
		t.Fatalf("format = %q, want \"\" (must not match on a partial prefix)", format)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, notGzip) {
		t.Error("passthrough mismatch for near-miss magic bytes")
	}
}

// TestCompressionExtensionsCoversEveryFormat keeps the list the UI builds its
// file picker from in step with the decoder. A codec added to
// compressionSuffixes without appearing here would leave the picker greying
// out files DecompressingReader handles perfectly well.
func TestCompressionExtensionsCoversEveryFormat(t *testing.T) {
	got := CompressionExtensions()

	seen := map[string]bool{}
	for _, ext := range got {
		if !strings.HasPrefix(ext, ".") {
			t.Errorf("%q is not a filename suffix; an HTML accept token needs the dot", ext)
		}
		if seen[ext] {
			t.Errorf("duplicate %q", ext)
		}
		seen[ext] = true
	}

	for format, sufs := range compressionSuffixes {
		for _, suf := range sufs {
			if !seen[suf] {
				t.Errorf("%s strips %q but does not advertise it", format, suf)
			}
		}
	}

	// Sorted, so the accept attribute is stable across builds rather than
	// reordering with Go's map iteration and churning every rendered page.
	if !sort.StringsAreSorted(got) {
		t.Errorf("not sorted: %v", got)
	}
}
