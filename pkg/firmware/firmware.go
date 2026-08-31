package firmware

// firmware.go contains the lifecycle Controller for host firmware delivery.
//
// Architecture:
//   - The BMC does NOT serve a bootable host image over the USB gadget. That
//     transport is retired: the host owns its own boot firmware.
//   - Updates are delivered as UEFI FMP capsules using the specification's
//     standard mechanism, "Delivering Capsules Across a System Reset"
//     (UEFI 2.10 §8.5.5). The BMC keeps a small GPT disk image at
//     c.capsulePath holding one EFI System Partition formatted FAT32, stages
//     capsules into \EFI\UpdateCapsule\ on it (see capsule.go), and presents
//     the image on the mass-storage gadget's lun.0. At the host's next boot
//     its firmware scans the attached FAT volumes, finds the capsules and
//     applies them via FMP.
//   - The whole volume is manipulated in userspace with go-diskfs. There is no
//     loop device, no kernel mount and no drop_caches cycle; the only kernel
//     interaction is clearing and re-setting lun.0's backing file so the host
//     sees a media change (see gadget.go).
//   - lun.0 stays writable (ro=0): host firmware deletes each capsule from
//     \EFI\UpdateCapsule\ once it has been applied, which is how the BMC can
//     tell an applied capsule from a pending one.
//
// The Controller is transport only: it moves capsules and virtual media onto
// the USB gadget for the host to consume, and never flashes the host itself.
// Boot overrides and host inventory are BMC state served over Redfish
// (api/redfish), which the host's firmware reads and applies itself.

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

// Status describes the current state of the firmware controller.
type Status struct {
	// VolumeReady reports whether the capsule volume exists on disk.
	VolumeReady bool `json:"volumeReady"`
	// Presented reports whether the capsule volume is on the gadget's lun.0.
	Presented bool `json:"presented"`
	// Staging reports whether a capsule fetch is currently running.
	Staging bool `json:"staging"`
	// CapsulePath is the on-BMC path of the capsule volume image.
	CapsulePath string `json:"capsulePath"`
	// CapsuleDir is the directory inside the volume the host firmware scans.
	CapsuleDir string `json:"capsuleDir"`
	// VolumeSize is the capsule volume's size in bytes (0 when not created).
	VolumeSize int64 `json:"volumeSize"`
	// Capsules are the capsules currently staged for the host.
	Capsules []Capsule `json:"capsules"`
}

// Controller manages capsule delivery and virtual media.
type Controller struct {
	mu sync.Mutex

	capsulePath string // GPT image presented on lun.0
	capsuleSize int64  // size used when the image is first created
	mediaDir    string // staging area for ISO files the user has uploaded

	presented bool

	vmState VirtualMediaState // current virtual media insertion state
	gadget  VMGadget          // test seam; nil means the usbgadget singleton

	log *slog.Logger
}

// NewController builds the firmware Controller from config. Called once by
// cmd/server/main.go; the returned Controller is then threaded to every
// consumer via pkg/deps instead of a package-level singleton.
func NewController(cfg *config.Config, log *slog.Logger) *Controller {
	return &Controller{
		capsulePath: cfg.Firmware.CapsulePath,
		capsuleSize: capsuleVolumeBytes(cfg.Firmware.CapsuleSizeMB),
		mediaDir:    cfg.Firmware.MediaDir,
		log:         logger.Or(log),
	}
}

// Init creates the capsule volume if it does not exist yet and presents it on
// the gadget's lun.0. Creating a 64 MiB FAT32 volume is fast (it writes a GPT,
// two FATs and a root directory, not the whole file), so unlike the retired
// boot-image seed this does not need to run in the background. Call once at
// server startup, after usbgadget.Get().Init has built the gadget.
func (c *Controller) Init(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Runs before the capsule work so its early returns can't skip the sweep.
	c.sweepEphemeralMediaLocked()

	if err := c.ensureVolumeLocked(); err != nil {
		return err
	}
	if err := c.presentVolume(ctx); err != nil {
		c.log.Warn("firmware: USB gadget present failed (may not be available in this environment)", slog.Any("err", err))
	}
	return nil
}

// GetStatus returns the current lifecycle state.
func (c *Controller) GetStatus() Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := Status{
		Presented:   c.presented,
		Staging:     isStaging(),
		CapsulePath: c.capsulePath,
		CapsuleDir:  capsuleDir,
		Capsules:    []Capsule{},
	}
	if size, ok := c.volumeSize(); ok {
		st.VolumeReady = true
		st.VolumeSize = size
	}
	if caps, err := c.listCapsulesLocked(); err == nil {
		st.Capsules = caps
	} else if st.VolumeReady {
		c.log.Warn("firmware: list capsules for status failed", slog.Any("err", err))
	}
	return st
}
