package firmware

// fetch.go pulls an FMP capsule from a URL and stages it for the host.
//
// The download lands in a temporary file on the BMC first and is only copied
// into the capsule volume once it is complete. Streaming the HTTP body
// straight into the FAT would hold lun.0 unpresented — the host staring at an
// empty drive — for the whole transfer, and a truncated transfer would leave a
// half-written capsule for firmware to trip over.
//
// That temporary file MUST live on the data partition. os.CreateTemp("") puts
// it in os.TempDir(), which on this device is the tmpfs overlay over the
// squashfs root — a few tens of megabytes of RAM. Downloading a capsule there
// filled the overlay and took the server down partway through the transfer,
// the same failure mode multipart uploads used to have (pkg/utils/fetch.go).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// maxCapsuleFetchBytes bounds a capsule download. It matches the caps on the
// capsule upload/push paths, and — unlike them — also protects the data
// partition the download is staged on, since the remote picks the size.
const maxCapsuleFetchBytes = 128 << 20 // 128 MiB

// IsStaging reports whether a capsule fetch is currently running. Callers use
// it to reject a second concurrent update rather than queue one.
func (c *Controller) IsStaging() bool { return isStaging() }

// StageCapsuleFromURL downloads the capsule at rawURL and stages it into
// \EFI\UpdateCapsule\ on the capsule volume. name overrides the filename
// derived from the URL path when non-empty. The host applies the capsule at
// its next boot; nothing is flashed here.
//
// ctx bounds the download. Callers run this detached from the HTTP request
// that asked for it — a capsule fetch outlives the 202 that acknowledged it —
// so ctx should be the process-lifetime context, not the request's. Cancelling
// it aborts the transfer and the staging file is removed by the deferred
// cleanup, leaving no half-written capsule for firmware to trip over.
func (c *Controller) StageCapsuleFromURL(ctx context.Context, rawURL, name string) (retErr error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("capsule URL must be http or https")
	}
	if name == "" {
		name = path.Base(parsed.Path)
	}
	fileName, err := capsuleFileName(name)
	if err != nil {
		return fmt.Errorf("cannot derive a capsule filename from %q: %w", rawURL, err)
	}

	if _, err := os.Stat(stagingSentinel); err == nil {
		return fmt.Errorf("a capsule is already being staged")
	}
	if err := os.WriteFile(stagingSentinel, []byte(fileName), 0o600); err != nil {
		return fmt.Errorf("create staging sentinel: %w", err)
	}
	defer os.Remove(stagingSentinel)

	started := time.Now()
	defer func() {
		outcome := "ok"
		if retErr != nil {
			outcome = "error"
		}
		telemetry.FirmwareDownload(ctx, outcome, time.Since(started).Seconds())
	}()

	// Beside the capsule volume on the data partition, never os.TempDir().
	stageDir, err := c.stagingDir()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stageDir, "capsule-*.bin")
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	slog.InfoContext(ctx, "firmware: downloading capsule", slog.String("name", fileName), slog.String("url", rawURL))
	if err := downloadTo(ctx, rawURL, tmp, maxCapsuleFetchBytes); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind staging file: %w", err)
	}

	written, err := c.StageCapsule(fileName, tmp)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "firmware: capsule staged; the host applies it at its next boot", slog.String("name", fileName), slog.Int64("bytes", written))
	return nil
}

// stagingDir returns the directory a capsule download is staged in: the
// directory holding the capsule volume, which is on the data partition. It is
// created if missing so a fetch works on a freshly flashed card.
func (c *Controller) stagingDir() (string, error) {
	if c.capsulePath == "" {
		return "", fmt.Errorf("capsulePath not configured")
	}
	dir := filepath.Dir(c.capsulePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	return dir, nil
}

// downloadTo copies the body at rawURL into w, refusing anything larger than
// maxBytes. utils.FetchURL owns the scheme check, the transport timeouts and
// the cap; everything here is a straight stream to disk.
func downloadTo(ctx context.Context, rawURL string, w io.Writer, maxBytes int64) error {
	remote, err := utils.FetchURL(ctx, rawURL, maxBytes)
	if err != nil {
		return err
	}
	defer remote.Close()

	written, err := io.Copy(w, remote)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	slog.InfoContext(ctx, "firmware: downloaded", slog.Int64("bytes", written))
	return nil
}
