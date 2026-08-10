package firmware

// firmware.go contains the lifecycle Controller for the firmware boot image.
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
//   - The U-Boot environment is NOT part of the image: it lives in a region
//     of the I2C EEPROM (the host's CONFIG_ENV_IS_IN_EEPROM partition, see
//     ubootenv.Store on c.env), so the BMC and U-Boot read and write the same
//     bytes and an image update never disturbs it. This replaced the earlier
//     machine.env / persistent.env / once.env files in the FAT image.
//   - c.firmwareDir is a host-side staging area mirroring files we want
//     to push into the image. SyncFirmwareDirToImage copies its contents
//     over the mounted image.

import (
	"fmt"
	"os"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
	"github.com/pi-bmc/nanokvm-app/pkg/ubootenv"
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

	// env is the U-Boot environment. It lives in a region of the I2C EEPROM
	// (the host's CONFIG_ENV_IS_IN_EEPROM partition), not in files inside the
	// boot image, so the BMC and U-Boot read and write the same bytes.
	// nil when unconfigured.
	env *ubootenv.Store

	presented bool

	reader  *readerCache      // cached read-only diskfs handle; nil = not open
	vmState VirtualMediaState // current virtual media insertion state
}

// NewController builds the firmware Controller from config. Called once by
// cmd/server/main.go; the returned Controller is then threaded to every
// consumer via pkg/deps instead of a package-level singleton.
func NewController(cfg *config.Config) *Controller {
	c := &Controller{
		imageURL:    cfg.Firmware.ImageURL,
		imagePath:   cfg.Firmware.ImagePath,
		seedPath:    cfg.Firmware.SeedPath,
		mountPoint:  cfg.Firmware.MountPoint,
		firmwareDir: cfg.Firmware.FirmwareDir,
		mediaDir:    cfg.Firmware.MediaDir,
	}
	c.env = newEnvStore(cfg.UbootEnv)
	return c
}

// newEnvStore builds the EEPROM-backed U-Boot environment store from config.
// The environment occupies [Offset, Offset+Size) of the same EEPROM that
// holds the UEFI variable store, so the backend spans up to the end of the
// env region. Returns nil when the store is disabled or unconfigured.
func newEnvStore(cfg config.UbootEnv) *ubootenv.Store {
	if !cfg.Enabled {
		return nil
	}
	// The efivars backends address the device with absolute offsets and match
	// ubootenv.Backend structurally, so both stores share one EEPROM device.
	var b ubootenv.Backend
	switch {
	case cfg.Path != "":
		b = efivars.NewFileBackend(cfg.Path, cfg.Offset+cfg.Size)
		log.Infof("ubootenv: using file store %s at offset %#x", cfg.Path, cfg.Offset)
	case cfg.I2CBus >= 0:
		b = efivars.NewI2CBackend(cfg.I2CBus, uint16(cfg.I2CAddr), //nolint:gosec // 7-bit address
			cfg.PageSize, cfg.Offset+cfg.Size)
		log.Infof("ubootenv: using i2c store bus %d addr %#x at offset %#x",
			cfg.I2CBus, cfg.I2CAddr, cfg.Offset)
	default:
		log.Warn("ubootenv: enabled but neither path nor i2c bus configured")
		return nil
	}
	return ubootenv.NewStore(b, cfg.Offset, cfg.Size, cfg.SnapshotPath)
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

	// Reconcile the durable env snapshot against the (volatile) EEPROM and
	// start watching for host-side saveenv writes. Best-effort.
	c.env.StartPersistence()
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

// ---- env API ---------------------------------------------------------------

// LoadEnv returns the U-Boot environment read from the EEPROM. It is the
// effective environment: U-Boot reads the same bytes at boot and rewrites
// them on saveenv.
func (c *Controller) LoadEnv() (*ubootenv.Env, error) {
	return c.env.Load()
}

// BootTargets bundles the boot-target views.
//
// The environment is a single store now that it lives in the EEPROM, so
// Persistent and Effective are the same boot_targets value — U-Boot reads the
// bytes the BMC wrote. Once is not an environment concept: one-shot overrides
// are UEFI BootNext, owned by the efivars store.
type BootTargets struct {
	Persistent string `json:"persistent"` // boot_targets in the EEPROM env
	Once       string `json:"once"`       // pending UEFI BootNext, if any
	Effective  string `json:"effective"`  // boot_targets in the EEPROM env
}

// GetBootTargets returns the persistent/effective boot_targets from the
// environment plus any pending one-shot UEFI BootNext.
func (c *Controller) GetBootTargets() (BootTargets, error) {
	env, err := c.LoadEnv()
	if err != nil {
		return BootTargets{}, err
	}
	targets, _ := env.Get(ubootenv.VarBootTargets)
	bt := BootTargets{Persistent: targets, Effective: targets}
	bt.Once = pendingBootNextTarget()
	return bt, nil
}

// pendingBootNextTarget reports a pending UEFI BootNext as a U-Boot
// boot_targets value, or "" when there is none (or no variable store).
func pendingBootNextTarget() string {
	mgr := efivars.GetManager()
	if !mgr.Available() {
		return ""
	}
	target, enabled, err := mgr.BootSourceOverride()
	if err != nil || enabled != "Once" || target == efivars.TargetUnknown {
		return ""
	}
	return RedfishToUBoot[string(target)]
}

// GetBootTarget returns the persistent boot_targets from the environment.
func (c *Controller) GetBootTarget() (string, error) {
	bt, err := c.GetBootTargets()
	return bt.Persistent, err
}

// GetOnceBootTarget returns the pending one-shot boot target (UEFI BootNext).
func (c *Controller) GetOnceBootTarget() (string, error) {
	bt, err := c.GetBootTargets()
	return bt.Once, err
}

// GetEffectiveBootTarget returns the effective boot_targets from the
// environment.
func (c *Controller) GetEffectiveBootTarget() (string, error) {
	bt, err := c.GetBootTargets()
	return bt.Effective, err
}

// SetBootTarget writes a persistent boot_targets override to the environment
// in the EEPROM. An empty value clears it.
func (c *Controller) SetBootTarget(targets string) error {
	return c.env.Update(func(env *ubootenv.Env) {
		if targets == "" {
			env.Delete(ubootenv.VarBootTargets)
			return
		}
		env.Set(ubootenv.VarBootTargets, targets)
	})
}

// SetBootTargetOnce applies a one-shot boot override. The environment has no
// apply-once semantics, so this drives UEFI BootNext through the variable
// store — the mechanism the host actually honours (bootcmd is `bootefi
// bootmgr`). An empty value clears any pending override.
func (c *Controller) SetBootTargetOnce(targets string) error {
	mgr := efivars.GetManager()
	if !mgr.Available() {
		return fmt.Errorf("one-shot boot override requires the UEFI variable store")
	}
	if targets == "" {
		return mgr.ClearBootSourceOverride()
	}
	rf, ok := UBootToRedfish[targets]
	if !ok {
		return fmt.Errorf("no UEFI boot target for boot_targets %q", targets)
	}
	return mgr.SetBootSourceOverride(efivars.BootTarget(rf), true)
}

// GetInventory returns board inventory data from the environment. Fresh read.
func (c *Controller) GetInventory() (map[string]string, error) {
	env, err := c.LoadEnv()
	if err != nil {
		return nil, err
	}
	return env.GetInventory(), nil
}

// GetAllEnvVars returns all variables from machine.env. Fresh read.
func (c *Controller) GetAllEnvVars() (map[string]string, error) {
	env, err := c.LoadEnv()
	if err != nil {
		return nil, err
	}
	return env.Vars, nil
}
