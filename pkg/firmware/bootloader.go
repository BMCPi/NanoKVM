package firmware

// bootloader.go assembles the running bootloader's provenance from the two
// out-of-band channels U-Boot publishes over I2C.
//
// The rpi-eeprom's identity — its build version and git hash — rides in the
// SMBIOS Type 45 Firmware Inventory (pkg/smbios). Those fields are
// free-form strings there, so the version is carried exactly rather than
// squeezed into the byte-wide release fields of SMBIOS Type 0 (which describes
// U-Boot itself, not the eeprom).
//
// The live EEPROM config and flash time still come from the UEFI variables
// (pkg/bootloader); U-Boot no longer dumps pieeprom.bin to the FAT,
// and the BMC owns the pieeprom.upd/.sig/recovery file lifecycle itself.

import (
	"errors"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/bootloader"
	"github.com/pi-bmc/nanokvm-app/pkg/smbios"
)

// eepromAppliedMarker is the file the Pi firmware leaves on the FAT after it
// applies a staged update: rpi-eeprom-update renames recovery.bin to
// recovery.000 on success. PREBOOT keyed its cleanup on this file; the BMC
// now does.
const eepromAppliedMarker = "recovery.000"

// BootloaderProvenance is the running bootloader's identity, assembled from the
// SMBIOS Type 45 firmware inventory and the UEFI variables U-Boot publishes.
type BootloaderProvenance struct {
	// Version is the rpi-eeprom build version as an ISO date "YYYY-MM-DD".
	// The eeprom is date-versioned, so its build date is its version. From
	// the SMBIOS Type 45 firmware inventory; empty when the host has not
	// published one.
	Version string
	// ReleaseDate is the component's release date as SMBIOS reports it
	// (an ISO date), or "" when absent.
	ReleaseDate string
	// GitVersion is the rpi-eeprom git hash (/chosen/bootloader/version),
	// distinct from the date-based Version. It comes from the Type 45
	// firmware ID; older U-Boot published it as a UEFI variable instead, so
	// that is kept as a fallback.
	GitVersion string
	// Config is the live EEPROM bootconf text (the running configuration).
	Config string
	// UpdatedUnix is when the EEPROM was last flashed; 0 when unknown.
	UpdatedUnix int64
}

// Available reports whether anything reported the bootloader's provenance.
func (p BootloaderProvenance) Available() bool {
	return p.Version != "" || p.GitVersion != "" || p.Config != "" ||
		p.UpdatedUnix != 0
}

// GetBootloaderProvenance merges the SMBIOS Type 45 firmware inventory (the
// version and git hash) with the UEFI variables (the live config and flash
// time). Each source is optional: a missing one leaves its fields empty rather
// than failing, so callers treat absent provenance as "not reported yet".
func (c *Controller) GetBootloaderProvenance() BootloaderProvenance {
	var prov BootloaderProvenance

	// SMBIOS Type 45 - the running firmware's version and identifier.
	if info, err := smbios.GetStore().Load(); err == nil &&
		len(info.FirmwareInventory) > 0 {
		fw := info.FirmwareInventory[0]
		prov.Version = fw.Version
		prov.ReleaseDate = fw.ReleaseDate
		prov.GitVersion = fw.ID
	}

	// UEFI variables - the live config and flash time (plus the git hash on
	// firmware that predates the Type 45 carriage).
	info, err := bootloader.GetStore().Load()
	if err != nil {
		if !errors.Is(err, bootloader.ErrNotConfigured) {
			log.Warnf("firmware: reading bootloader variables: %v", err)
		}
		return prov
	}
	prov.Config = info.Config
	if !info.UpdatedAt.IsZero() {
		prov.UpdatedUnix = info.UpdatedAt.Unix()
	}
	if prov.GitVersion == "" {
		prov.GitVersion = info.Version
	}
	return prov
}

// reconcilePieepromFiles removes staged EEPROM update files once the firmware
// has applied them. rpi-eeprom-update renames recovery.bin to recovery.000
// after a successful flash, so that marker's presence means the pieeprom.upd
// and pieeprom.sig the BMC staged have been consumed.
//
// Best-effort and idempotent: no marker is the common (nothing applied) case,
// and removing an already-absent file is not an error.
func (c *Controller) reconcilePieepromFiles() {
	if ok, _ := c.hasFileOnImage(eepromAppliedMarker); !ok {
		return
	}
	for _, name := range []string{eepromAppliedMarker, eepromPendingFile, eepromPendingSigFile} {
		if err := c.RemoveFileFromImage(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warnf("firmware: removing applied %s: %v", name, err)
		}
	}
	log.Info("firmware: cleaned up applied EEPROM update files")
}
