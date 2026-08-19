package firmware

// fetch.go pulls an FMP capsule from a URL and stages it for the host.
//
// The download lands in a temporary file on the BMC first and is only copied
// into the capsule volume once it is complete. Streaming the HTTP body
// straight into the FAT would hold lun.0 unpresented — the host staring at an
// empty drive — for the whole transfer, and a truncated transfer would leave a
// half-written capsule for firmware to trip over.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
)

// fetchTimeout bounds a single capsule download. Capsules are firmware-sized
// (single-digit MB), so a slow link still finishes well inside this.
const fetchTimeout = 15 * time.Minute

// IsStaging reports whether a capsule fetch is currently running. Callers use
// it to reject a second concurrent update rather than queue one.
func (c *Controller) IsStaging() bool { return isStaging() }

// StageCapsuleFromURL downloads the capsule at rawURL and stages it into
// \EFI\UpdateCapsule\ on the capsule volume. name overrides the filename
// derived from the URL path when non-empty. The host applies the capsule at
// its next boot; nothing is flashed here.
func (c *Controller) StageCapsuleFromURL(rawURL, name string) (retErr error) {
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
		telemetry.FirmwareDownload(context.Background(), outcome, time.Since(started).Seconds())
	}()

	tmp, err := os.CreateTemp("", "capsule-*.bin")
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	log.Infof("firmware: downloading capsule %s from %s", fileName, rawURL)
	if err := downloadTo(rawURL, tmp); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind staging file: %w", err)
	}

	written, err := c.StageCapsule(fileName, tmp)
	if err != nil {
		return err
	}
	log.Infof("firmware: capsule %s staged (%d bytes); the host applies it at its next boot", fileName, written)
	return nil
}

// downloadTo copies the body at rawURL into w.
func downloadTo(rawURL string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}
	written, err := io.Copy(w, resp.Body)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	log.Infof("firmware: downloaded %d bytes", written)
	return nil
}
