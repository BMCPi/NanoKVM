package firmware

// firmware.go contains the lifecycle Controller for the host boot image.
//
// Architecture:
//   - The image at c.imagePath is the canonical, bootable artefact. It is
//     downloaded as-is from c.imageURL (xz-compressed) on first run.
//   - The image is presented unchanged to the USB mass-storage gadget via
//     /sys/kernel/config/usb_gadget/g0/.../lun.0/file.
//   - All read/write access to the image's filesystem goes through a
//     mount cycle inside withMount(): unpresent → mount (offset-based loop) →
//     fn → sync → umount → drop_caches → present. No persistent loop device
//     is maintained; the kernel handles loop attachment internally as part of
//     `mount -o loop,offset=...`.
//   - c.firmwareDir is a host-side staging area mirroring files we want
//     to push into the image. SyncFirmwareDirToImage copies its contents
//     over the mounted image.
//
// The Controller is transport only: it moves images and media onto the USB
// gadget for the host to consume. Boot overrides and host inventory are BMC
// state served over Redfish (api/redfish), which the host's firmware reads
// and applies itself — the BMC never edits a boot environment for it.

import (
	"os"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// Status describes the current state of the firmware controller.
type Status struct {
	Downloaded    bool   `json:"downloaded"`
	Downloading   bool   `json:"downloading"`
	Presented     bool   `json:"presented"`
	ImagePath     string `json:"imagePath"`
	MountPoint    string `json:"mountPoint"`
	FirmwareDir   string `json:"firmwareDir"`
	FirmwareCount int    `json:"firmwareCount"`
}

// Controller manages the firmware image lifecycle.
type Controller struct {
	mu sync.Mutex

	imageURL    string
	imagePath   string
	seedPath    string // baked-in .xz seed tried before any download
	mountPoint  string
	firmwareDir string
	mediaDir    string // staging area for ISO files the user has uploaded

	presented bool

	reader  *readerCache      // cached read-only diskfs handle; nil = not open
	vmState VirtualMediaState // current virtual media insertion state
}

// NewController builds the firmware Controller from config. Called once by
// cmd/server/main.go; the returned Controller is then threaded to every
// consumer via pkg/deps instead of a package-level singleton.
func NewController(cfg *config.Config) *Controller {
	return &Controller{
		imageURL:    cfg.Firmware.ImageURL,
		imagePath:   cfg.Firmware.ImagePath,
		seedPath:    cfg.Firmware.SeedPath,
		mountPoint:  cfg.Firmware.MountPoint,
		firmwareDir: cfg.Firmware.FirmwareDir,
		mediaDir:    cfg.Firmware.MediaDir,
	}
}

// Init presents the boot image via the USB gadget when it exists; when it
// does not (factory-fresh data partition), the image is produced in the
// background — seeded from the baked-in copy, else downloaded — and
// presented on completion. Startup must not block on this: the seed is a
// ~513 MB decompress to SD that takes minutes, and the HTTP listeners only
// open after initialization, which is exactly the window that makes a
// first-boot BMC look dead. Call once at server startup.
func (c *Controller) Init() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.imageExists() {
		log.Infof("firmware: image not found at %s; producing it in the background", c.imagePath)
		go c.ensureImageAndPresent()
	} else {
		// The gadget itself (g0, all functions incl. lun.1) is built by the
		// usbgadget package at server startup, before this runs. Here we only
		// fill lun.0's backing file with the boot image.
		log.Info("firmware: image found, presenting via USB gadget")
		if err := c.presentImage(); err != nil {
			log.Warnf("firmware: USB gadget present failed (may not be available in this environment): %v", err)
		}
	}
	return nil
}

// ensureImageAndPresent seeds (else downloads) the boot image and presents
// it via the gadget. Runs in the background at startup; mirrors
// DownloadAndInit's locking.
func (c *Controller) ensureImageAndPresent() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.imageExists() {
		if err := c.seedImageLocked(); err != nil {
			log.Infof("firmware: no usable seed (%v); downloading", err)
			if err := c.downloadImageLocked(); err != nil {
				log.Errorf("firmware: ensure image: %v", err)
				return
			}
		}
	}
	if err := c.presentImage(); err != nil {
		log.Warnf("firmware: USB gadget present failed (may not be available in this environment): %v", err)
	}
}

// GetStatus returns the current lifecycle state.
func (c *Controller) GetStatus() Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	if entries, err := os.ReadDir(c.firmwareDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				count++
			}
		}
	}

	return Status{
		Downloaded:    c.imageExists(),
		Downloading:   c.IsDownloading(),
		Presented:     c.presented,
		ImagePath:     c.imagePath,
		MountPoint:    c.mountPoint,
		FirmwareDir:   c.firmwareDir,
		FirmwareCount: count,
	}
}

func (c *Controller) imageExists() bool {
	info, err := os.Stat(c.imagePath)
	return err == nil && info.Size() > 0
}
