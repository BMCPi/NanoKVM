package firmware

// update.go orchestrates u-boot firmware updates: query the latest release,
// determine whether an update is needed, and download/activate the new image.
// The U-Boot environment lives in the I2C EEPROM (see ubootenv.Store), which
// an image swap does not touch, so no env files are carried across updates.

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

// VersionInfo describes the current and latest u-boot versions.
type VersionInfo struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"updateAvailable"`
	AssetURL        string `json:"assetUrl,omitempty"`
}

// GetUBootVersionInfo returns the currently-running u-boot version (read
// from the environment's `ver` variable) and the latest available release.
func (c *Controller) GetUBootVersionInfo() (VersionInfo, error) {
	current := ""
	if env, err := c.LoadEnv(); err == nil {
		if v, ok := env.Get("ver"); ok {
			current = parseUBootVer(v)
		}
	}

	info := VersionInfo{Current: current}
	rel, err := LatestUBootRelease()
	if err != nil {
		return info, err
	}
	info.Latest = rel.Version
	info.AssetURL = rel.AssetURL
	if current == "" {
		info.UpdateAvailable = true
	} else {
		info.UpdateAvailable = CompareUBootVersions(rel.Version, current) > 0
	}
	return info, nil
}

// parseUBootVer extracts a "vMAJOR.MINOR[-rcN]" token from U-Boot's
// `ver` env variable, which typically looks like:
//
//	"U-Boot 2026.07-rc1 (Aug 28 2025 - 12:34:56 +0000)"
//	"U-Boot v2026.07 (...)"
//
// Returns the version with a leading "v" so it compares cleanly against
// release tags.
func parseUBootVer(s string) string {
	for _, tok := range strings.Fields(s) {
		t := strings.TrimPrefix(tok, "v")
		if t == "" {
			continue
		}
		if t[0] < '0' || t[0] > '9' {
			continue
		}
		// Looks like a version token (starts with a digit, contains a dot).
		if strings.Contains(t, ".") {
			return "v" + t
		}
	}
	return ""
}

// UpdateUBoot downloads the latest u-boot image and installs it,
// preserving the three env files from the existing image. If url is
// empty, the latest release URL is resolved automatically.
func (c *Controller) UpdateUBoot() error {
	rel, err := LatestUBootRelease()
	if err != nil {
		return fmt.Errorf("resolve latest release: %w", err)
	}
	return c.UpdateUBootFromURL(rel.AssetURL)
}

// UpdateUBootFromURL replaces the current image with the .img.xz at the
// given URL, preserving env files.
func (c *Controller) UpdateUBootFromURL(url string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if url == "" {
		return fmt.Errorf("empty url")
	}

	// Download & install the new image (replaces c.imagePath atomically). The
	// U-Boot environment lives in the EEPROM, which an image swap does not
	// touch, so nothing has to be preserved across the update.
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

// ActivateVersionedImage swaps the versioned image for the given u-boot
// version into the active image slot (c.imagePath), preserving the three
// env files from the current active image. The versioned cache file is kept
// so it can be re-activated later without re-downloading.
func (c *Controller) ActivateVersionedImage(version string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	srcPath := c.versionedImagePath(version)
	if info, err := os.Stat(srcPath); err != nil || info.Size() == 0 {
		return fmt.Errorf("versioned image for %s not found; download it first", version)
	}

	// Swap the versioned image into the active slot. The U-Boot environment
	// lives in the EEPROM, which the swap does not touch, so nothing has to be
	// preserved across the activation.
	if err := c.swapActiveLocked(srcPath); err != nil {
		return err
	}

	log.Infof("firmware: activated versioned image %s → %s", version, c.imagePath)

	// Persist the activated version so the kernels endpoint can report the
	// correct active entry even before the board reboots (the environment
	// still holds the old U-Boot ver string at this point).
	trackFile := filepath.Join(filepath.Dir(c.imagePath), ".activated-uboot-version")
	if err := os.WriteFile(trackFile, []byte(version), 0o644); err != nil {
		log.Warnf("firmware: write activated-version tracking file: %v", err)
	}

	InvalidateLatestUBootCache()
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
