package firmware

// download.go fetches the upstream U-Boot image (xz-compressed) and writes
// it directly to c.imagePath. No on-the-fly image construction is performed:
// the downloaded image is the canonical bootable artefact, byte-for-byte.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/ulikunitz/xz"

	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
)

const downloadSentinel = "/tmp/.firmware_download_in_progress"

// Bootstrap downloads the upstream image (and decompresses if .xz) to
// c.imagePath. Idempotent: overwrites any existing image.
func (c *Controller) Bootstrap() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.downloadImageLocked()
}

// DownloadAndInit bootstraps from the upstream image, then presents via gadget.
func (c *Controller) DownloadAndInit() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.downloadImageLocked(); err != nil {
		return err
	}
	if err := c.presentImage(); err != nil {
		log.Warnf("firmware: USB gadget present failed: %v", err)
	}
	return nil
}

// IsDownloading returns true if a download is in progress.
func (c *Controller) IsDownloading() bool {
	_, err := os.Stat(downloadSentinel)
	return err == nil
}

// downloadImageLocked downloads c.imageURL to c.imagePath. Must hold c.mu.
// Unpresents the gadget for the duration so writes to c.imagePath don't
// race with the gadget's view of the file.
func (c *Controller) downloadImageLocked() (retErr error) {
	if _, err := os.Stat(downloadSentinel); err == nil {
		return fmt.Errorf("download already in progress")
	}
	if err := os.WriteFile(downloadSentinel, []byte("downloading"), 0o644); err != nil {
		return fmt.Errorf("create sentinel: %w", err)
	}
	defer os.Remove(downloadSentinel)

	started := time.Now()
	defer func() {
		outcome := "ok"
		if retErr != nil {
			outcome = "error"
		}
		telemetry.FirmwareDownload(context.Background(), outcome, time.Since(started).Seconds())
	}()

	if c.imageURL == "" {
		return fmt.Errorf("imageURL not configured")
	}
	if err := os.MkdirAll(filepath.Dir(c.imagePath), 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}

	// Release any gadget hold on the existing image.
	wasPresented := c.presented
	if wasPresented {
		if err := c.unpresentImage(); err != nil {
			log.Warnf("firmware: pre-download unpresent failed: %v", err)
		}
	}
	c.invalidateReaderCacheLocked()

	stageDir := filepath.Join(filepath.Dir(c.imagePath), "stage")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}
	xzPath := filepath.Join(stageDir, "upstream.img.xz")
	imgPath := filepath.Join(stageDir, "upstream.img")
	defer func() {
		_ = os.Remove(xzPath)
		_ = os.Remove(imgPath)
	}()

	log.Infof("firmware: downloading %s", c.imageURL)
	if err := downloadFile(c.imageURL, xzPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	log.Info("firmware: decompressing image")
	if err := decompressXZ(xzPath, imgPath); err != nil {
		return fmt.Errorf("decompress: %w", err)
	}

	// Atomically replace the destination image.
	if err := moveFile(imgPath, c.imagePath); err != nil {
		return fmt.Errorf("install image: %w", err)
	}
	_ = exec.Command("sync").Run()

	log.Infof("firmware: installed image at %s", c.imagePath)

	// Re-present the (new) image for the gadget.
	if wasPresented {
		if err := c.presentImage(); err != nil {
			log.Warnf("firmware: post-download present failed: %v", err)
		}
	}
	return nil
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	written, err := io.Copy(f, resp.Body)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	log.Infof("firmware: downloaded %d bytes", written)
	return f.Sync()
}

// seedImageLocked installs the boot image from the xz seed baked into the
// read-only rootfs (rpi-firmware-seed recipe; path from Firmware.SeedPath),
// replacing the retired S06nanokvm-data init script. Must hold c.mu. Same
// decompress-to-temp + same-dir rename contract as downloads, so a power
// cut can never leave a truncated image that imageExists()'s size check
// would accept.
func (c *Controller) seedImageLocked() error {
	if c.seedPath == "" {
		return fmt.Errorf("no seed configured")
	}
	if fi, err := os.Stat(c.seedPath); err != nil {
		return err
	} else if fi.Size() == 0 {
		return fmt.Errorf("seed %s is empty", c.seedPath)
	}
	if err := os.MkdirAll(filepath.Dir(c.imagePath), 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	tmp := c.imagePath + ".seed.tmp"
	defer os.Remove(tmp)
	log.Infof("firmware: seeding image from %s", c.seedPath)
	if err := decompressXZ(c.seedPath, tmp); err != nil {
		return err
	}
	if err := moveFile(tmp, c.imagePath); err != nil {
		return fmt.Errorf("install seeded image: %w", err)
	}
	log.Infof("firmware: seeded %s", c.imagePath)
	return nil
}

func decompressXZ(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open xz: %w", err)
	}
	defer in.Close()

	r, err := xz.NewReader(in)
	if err != nil {
		return fmt.Errorf("xz reader: %w", err)
	}

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, r); err != nil {
		return fmt.Errorf("xz decompress: %w", err)
	}
	return out.Sync()
}

// moveFile renames src to dest, falling back to copy+remove for cross-FS moves.
func moveFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
