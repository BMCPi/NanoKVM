package eepromupdater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleVersionsTxt = `# firmware-2712 firmware versions
#
# version   build_epoch  fw_git_hash  release  mfg_ver
2026-05-22  1779408415   7dcdc4b8     latest   1
2026-05-17  1778976445   1abffaec     latest   1
2026-05-11  1778498402   66f33f7e     default  1
2026-04-30  1777551683   1a17f6cb     latest
2025-12-08  1765222194   2226a853     default
2025-11-27  1764250826   999d0ec9     old
`

// withRawBase points the package at srv for the duration of t.
func withRawBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := rawBase
	rawBase = srv.URL
	t.Cleanup(func() { rawBase = prev })
}

func TestFindLatest_StableUsesDefaultRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/firmware-2712/versions.txt") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(sampleVersionsTxt))
	}))
	defer srv.Close()
	withRawBase(t, srv)

	img, err := FindLatest(context.Background(), FindLatestOptions{
		Platform: PlatformRPi5,
		Channel:  ChannelStable,
	})
	if err != nil {
		t.Fatalf("FindLatest: %v", err)
	}
	if img.Version != "2026-05-11" {
		t.Errorf("Version = %q; want newest default 2026-05-11", img.Version)
	}
	if img.Name != "pieeprom-2026-05-11.bin" {
		t.Errorf("Name = %q", img.Name)
	}
	if !strings.HasSuffix(img.URL, "/firmware-2712/default/pieeprom-2026-05-11.bin") {
		t.Errorf("URL = %q", img.URL)
	}
	if !strings.HasSuffix(img.RecoveryURL, "/firmware-2712/default/recovery.bin") {
		t.Errorf("RecoveryURL = %q", img.RecoveryURL)
	}
}

func TestFindLatest_BetaUsesLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleVersionsTxt))
	}))
	defer srv.Close()
	withRawBase(t, srv)

	img, err := FindLatest(context.Background(), FindLatestOptions{
		Platform: PlatformRPi5,
		Channel:  ChannelBeta,
	})
	if err != nil {
		t.Fatalf("FindLatest: %v", err)
	}
	if img.Version != "2026-05-22" {
		t.Errorf("Version = %q; want newest latest 2026-05-22", img.Version)
	}
	if !strings.Contains(img.URL, "/firmware-2712/latest/") {
		t.Errorf("URL = %q; want latest release dir", img.URL)
	}
}

func TestFindLatest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	withRawBase(t, srv)

	if _, err := FindLatest(context.Background(), FindLatestOptions{}); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestFindLatest_NoMatchingRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# only-old\n2024-01-01  0  abc  old  1\n"))
	}))
	defer srv.Close()
	withRawBase(t, srv)

	if _, err := FindLatest(context.Background(), FindLatestOptions{}); err == nil {
		t.Fatal("expected error when no default-release entries exist")
	}
}

func TestDownload_VerifiesSize(t *testing.T) {
	body := []byte("not-really-an-eeprom-image")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	img := &Image{Name: "pieeprom-test.bin", URL: srv.URL, Size: int64(len(body))}
	got, err := Download(context.Background(), img, srv.Client())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q want %q", got, body)
	}
}

func TestDownload_RejectsSizeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	img := &Image{Name: "pieeprom-test.bin", URL: srv.URL, Size: 1000}
	if _, err := Download(context.Background(), img, srv.Client()); err == nil {
		t.Fatal("expected size-mismatch error")
	}
}

func TestChannelRelease(t *testing.T) {
	cases := map[Channel]string{
		ChannelStable:   "default",
		ChannelBeta:     "latest",
		ChannelCritical: "default",
		"":              "default",
	}
	for c, want := range cases {
		if got := c.release(); got != want {
			t.Errorf("Channel(%q).release() = %q; want %q", c, got, want)
		}
	}
}
