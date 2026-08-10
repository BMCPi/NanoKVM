package redfish

// trusted_components.go exposes the RPi 5 bootloader (rpi-eeprom) as a Redfish
// TrustedComponent — the platform root of trust — with its running firmware as
// a nested SoftwareInventory. Everything comes through
// firmware.Controller.GetBootloaderProvenance, which merges the SMBIOS Type 45
// firmware inventory (the rpi-eeprom build date as an ISO "YYYY-MM-DD", plus
// its git hash) with the UEFI variables (the live config and flash time).
//
// Shape (all reachable from ComputerSystem.Links.TrustedComponents):
//
//	/redfish/v1/Systems/1/TrustedComponents                     collection
//	/redfish/v1/Systems/1/TrustedComponents/Bootloader          the RoT
//	/redfish/v1/Systems/1/TrustedComponents/Bootloader/SoftwareImages/Active
//	                                                            its firmware

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// bootloaderProvenance reads the running bootloader's version/config/flash-time
// from the firmware controller. Returns a zero value when U-Boot has not
// published the variables yet; the resources still exist (the RoT is physical),
// they just omit the version until it is reported.
func bootloaderProvenance(fw *firmware.Controller) firmware.BootloaderProvenance {
	return fw.GetBootloaderProvenance()
}

// bootloaderFirmware returns the running bootloader's version and release date
// for the Redfish resources. The version now rides in the SMBIOS tables as the
// rpi-eeprom build date rendered "YYYY.MDD"; it falls back to the git hash from
// the UEFI provenance variables for firmware that predates the SMBIOS carriage.
// releaseDate is the SMBIOS build date (RFC 3339), or the UEFI flash time as a
// fallback. Either value may be "" when nothing has been reported yet.
func bootloaderFirmware(prov firmware.BootloaderProvenance) (version, releaseDate string) {
	version = prov.Version
	if version == "" {
		version = prov.GitVersion // pre-Type-45 firmware
	}

	// Type 45 reports an ISO date; Redfish wants RFC 3339. Fall back to the
	// UEFI flash time when the host published no release date.
	if t, err := time.Parse("2006-01-02", prov.ReleaseDate); err == nil {
		releaseDate = t.UTC().Format(time.RFC3339)
	} else if prov.UpdatedUnix != 0 {
		releaseDate = time.Unix(prov.UpdatedUnix, 0).UTC().Format(time.RFC3339)
	}
	return version, releaseDate
}

func (s *Service) GetTrustedComponentCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"TrustedComponentCollection", "Trusted Component Collection",
		trustedComponentsPath,
		Link(bootloaderComponentPath),
	))
}

func (s *Service) GetTrustedComponentBootloader(c *gin.Context) {
	prov := bootloaderProvenance(s.Firmware)
	version, _ := bootloaderFirmware(prov)

	activeImage := Link(bootloaderSoftwarePath)
	integratedInto := Link(systemPath)

	c.JSON(http.StatusOK, TrustedComponent{
		Resource: Resource{
			ODataType:    "#TrustedComponent.v1_4_0.TrustedComponent",
			ODataID:      bootloaderComponentPath,
			ODataContext: context("TrustedComponent.TrustedComponent"),
			ID:           "Bootloader",
			Name:         "Bootloader EEPROM",
			Description:  "Raspberry Pi 5 bootloader (rpi-eeprom) — the platform root of trust.",
		},
		// Integrated: the loader lives in the SoC's boot SPI flash, not a
		// discrete pluggable part.
		TrustedComponentType: schemas.IntegratedTrustedComponentType,
		Manufacturer:         "Raspberry Pi",
		Model:                "RPi5 Bootloader EEPROM",
		FirmwareVersion:      version,
		Status:               &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		Links: &TrustedComponentLinks{
			ActiveSoftwareImage: &activeImage,
			SoftwareImages:      Links{activeImage},
			IntegratedInto:      &integratedInto,
		},
	})
}

func (s *Service) GetBootloaderSoftwareInventory(c *gin.Context) {
	prov := bootloaderProvenance(s.Firmware)
	version, releaseDate := bootloaderFirmware(prov)

	inv := SoftwareInventory{
		Resource: Resource{
			ODataType:    "#SoftwareInventory.v1_8_0.SoftwareInventory",
			ODataID:      bootloaderSoftwarePath,
			ODataContext: context("SoftwareInventory.SoftwareInventory"),
			ID:           "Active",
			Name:         "Bootloader Firmware",
			Description:  "Running rpi-eeprom bootloader image.",
		},
		SoftwareID: "rpi-eeprom",
		Version:    version,
		// The version is the rpi-eeprom build date ("YYYY-MM-DD") — or a git
		// hash from older firmware — neither is semantic versioning.
		VersionScheme: schemas.OEMVersionScheme,
		// The BMC stages pieeprom.upd for the host's rpi-eeprom-update to
		// flash on next boot (see firmware.SetEEPROMConfig).
		Updateable: true,
		Status:     &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	}
	if releaseDate != "" {
		inv.ReleaseDate = releaseDate
	}

	c.JSON(http.StatusOK, inv)
}
