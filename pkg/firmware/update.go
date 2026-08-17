package firmware

// update.go moves host boot images: download an image from a URL into the
// active slot the USB gadget presents, plus a versioned image cache with
// explicit activation. The BMC never flashes the host — an image swap only
// changes what the gadget offers, and the host consumes it at its own boot.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/ulikunitz/xz"
)

// UpdateHostImageFromURL replaces the current host boot image with the
// .img.xz at the given URL and re-presents it on the USB gadget.
func (c *Controller) UpdateHostImageFromURL(url string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if url == "" {
		return fmt.Errorf("empty url")
	}

	// Download & install the new image (replaces c.imagePath atomically).
	return c.downloadFromURLLocked(url)
}

// downloadFromURLLocked is identical to downloadImageLocked but takes an
// explicit URL (used by the upgrade flow). Must hold c.mu.
func (c *Controller) downloadFromURLLocked(url string) error {
	if _, err := os.Stat(downloadSentinel); err == nil {
		return fmt.Errorf("download already in progress")
	}
	if err := os.WriteFile(downloadSentinel, []byte("downloading"), 0o644); err != nil {
		return fmt.Errorf("create sentinel: %w", err)
	}
	defer os.Remove(downloadSentinel)

	if err := os.MkdirAll(filepath.Dir(c.imagePath), 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}

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

	log.Infof("firmware: downloading u-boot image from %s", url)
	if err := downloadFileTo(url, xzPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	log.Info("firmware: decompressing image")
	if err := decompressXZTo(xzPath, imgPath); err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	if err := moveFile(imgPath, c.imagePath); err != nil {
		return fmt.Errorf("install image: %w", err)
	}
	_ = exec.Command("sync").Run()
	log.Infof("firmware: installed image at %s", c.imagePath)

	if wasPresented {
		if err := c.presentImage(); err != nil {
			log.Warnf("firmware: post-download present failed: %v", err)
		}
	}
	return nil
}

// downloadFileTo is exported-style helper used by downloadFromURLLocked.
// It mirrors downloadFile() in download.go but with a parameterised URL.
func downloadFileTo(url, dest string) error {
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

func decompressXZTo(src, dest string) error {
	// Prefer the native xz binary if available — pure-Go xz decoding is
	// very slow on embedded RISC-V (multi-minute) for typical u-boot images.
	if xzBin, err := exec.LookPath("xz"); err == nil {
		log.Infof("firmware: decompressing with %s", xzBin)
		out, err := os.Create(dest)
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		defer out.Close()
		cmd := exec.Command(xzBin, "-dc", "--", src)
		cmd.Stdout = out
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("xz decompress: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		if err := out.Sync(); err != nil {
			return fmt.Errorf("sync output: %w", err)
		}
		if st, err := os.Stat(dest); err == nil {
			log.Infof("firmware: decompressed %d bytes", st.Size())
		}
		return nil
	}

	log.Info("firmware: native xz unavailable, falling back to pure-Go decoder (slow)")
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
	written, err := io.Copy(out, r)
	if err != nil {
		return fmt.Errorf("xz decompress: %w", err)
	}
	log.Infof("firmware: decompressed %d bytes", written)
	return out.Sync()
}

// ---------------------------------------------------------------------------
// Versioned image management
// ---------------------------------------------------------------------------

// versionedImagePath returns the on-disk path for a versioned u-boot image.
// e.g. version "v2026.04" → "/var/lib/nanokvm/firmware/uboot-v2026.04.img".
func (c *Controller) versionedImagePath(version string) string {
	// Normalise: ensure leading "v", replace any path-unsafe characters.
	v := version
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	v = strings.NewReplacer("/", "-", " ", "-", ":", "-").Replace(v)
	return filepath.Join(filepath.Dir(c.imagePath), "uboot-"+v+".img")
}

// VersionedImageExists reports whether a locally cached versioned image for
// the given u-boot version exists on disk.
func (c *Controller) VersionedImageExists(version string) bool {
	p := c.versionedImagePath(version)
	info, err := os.Stat(p)
	return err == nil && info.Size() > 0
}

// DeleteVersionedImage removes the locally cached image for the given u-boot
// version. After calling this, DownloadVersionedImage will re-fetch from
// upstream. No-op if the file does not exist.
func (c *Controller) DeleteVersionedImage(version string) {
	p := c.versionedImagePath(version)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warnf("firmware: delete versioned image %s: %v", version, err)
	}
}

// DownloadVersionedImage fetches and decompresses the u-boot image for the
// given version+URL into a versioned cache file (e.g. uboot-v2026.04.img).
// It does NOT replace the currently active image. Idempotent: if the file
// already exists it returns immediately. Safe to call from a goroutine.
func (c *Controller) DownloadVersionedImage(version, assetURL string) error {
	// Quick existence check before acquiring the sentinel.
	destPath := c.versionedImagePath(version)
	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		log.Infof("firmware: versioned image for %s already cached at %s", version, destPath)
		return nil
	}

	// Use the shared sentinel so versioned and active-image downloads are
	// mutually exclusive (prevents bandwidth/disk contention).
	if _, err := os.Stat(downloadSentinel); err == nil {
		return fmt.Errorf("download already in progress")
	}
	if err := os.WriteFile(downloadSentinel, []byte("downloading"), 0o644); err != nil {
		return fmt.Errorf("create sentinel: %w", err)
	}
	defer os.Remove(downloadSentinel)

	imageDir := filepath.Dir(c.imagePath)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}

	stageDir := filepath.Join(imageDir, "stage")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}
	// Use a version-specific stage name to avoid collisions.
	safeVer := strings.NewReplacer("/", "-", " ", "-", ":", "-").Replace(version)
	xzPath := filepath.Join(stageDir, "ver-"+safeVer+".img.xz")
	imgPath := filepath.Join(stageDir, "ver-"+safeVer+".img")
	defer func() {
		_ = os.Remove(xzPath)
		_ = os.Remove(imgPath)
	}()

	log.Infof("firmware: downloading versioned u-boot %s from %s", version, assetURL)
	if err := downloadFileTo(assetURL, xzPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	log.Infof("firmware: decompressing versioned image for %s", version)
	if err := decompressXZTo(xzPath, imgPath); err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	if err := copyFileContents(imgPath, destPath); err != nil {
		return fmt.Errorf("install versioned image: %w", err)
	}
	_ = exec.Command("sync").Run()
	log.Infof("firmware: versioned image %s stored at %s", version, destPath)
	return nil
}

// ActivateVersionedImage swaps the versioned image for the given version
// into the active image slot (c.imagePath). The versioned cache file is
// kept so it can be re-activated later without re-downloading.
func (c *Controller) ActivateVersionedImage(version string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	srcPath := c.versionedImagePath(version)
	if info, err := os.Stat(srcPath); err != nil || info.Size() == 0 {
		return fmt.Errorf("versioned image for %s not found; download it first", version)
	}

	// Swap the versioned image into the active slot.
	if err := c.swapActiveLocked(srcPath); err != nil {
		return err
	}

	log.Infof("firmware: activated versioned image %s → %s", version, c.imagePath)

	// Persist the activated version so the overview can report the correct
	// active entry across BMC restarts.
	trackFile := filepath.Join(filepath.Dir(c.imagePath), ".activated-uboot-version")
	if err := os.WriteFile(trackFile, []byte(version), 0o644); err != nil {
		log.Warnf("firmware: write activated-version tracking file: %v", err)
	}

	return nil
}

// ActiveUBootVersion returns the U-Boot version most recently activated via
// ActivateVersionedImage. The value is read from a small tracking file so it
// survives server restarts. Returns "" if no versioned activation has ever
// been performed (e.g. the active image was installed via UpdateUBoot).
func (c *Controller) ActiveUBootVersion() string {
	trackFile := filepath.Join(filepath.Dir(c.imagePath), ".activated-uboot-version")
	data, err := os.ReadFile(trackFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// swapActiveLocked copies srcPath over c.imagePath, handling gadget/loop// bookkeeping. Must hold c.mu.
func (c *Controller) swapActiveLocked(srcPath string) error {
	wasPresented := c.presented
	if wasPresented {
		if err := c.unpresentImage(); err != nil {
			log.Warnf("firmware: pre-activate unpresent: %v", err)
		}
	}
	c.invalidateReaderCacheLocked()

	if err := copyFileContents(srcPath, c.imagePath); err != nil {
		// Best-effort restore of gadget state before returning the error.
		if wasPresented {
			_ = c.presentImage()
		}
		return fmt.Errorf("swap active image: %w", err)
	}
	_ = exec.Command("sync").Run()

	if wasPresented {
		if err := c.presentImage(); err != nil {
			log.Warnf("firmware: post-activate present: %v", err)
		}
	}
	return nil
}

// copyFileContents copies src to dst byte-for-byte, creating/overwriting dst.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync: %w", err)
	}
	return out.Close()
}
