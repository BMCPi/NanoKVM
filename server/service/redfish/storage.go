package redfish

// storage.go exposes the storage the BMC itself presents to the host — the
// USB mass-storage gadget's LUNs — as the standard Storage subsystem
// (/redfish/v1/Systems/1/Storage/1) with one Drive per backed LUN.
//
// The linking model follows JetKVM's service (a StorageCollection with a
// single subsystem whose Drives array links Drive resources), but where
// JetKVM must have its host POST drive inventory in through the host
// interface (SMBIOS defines no structure for disks, so the BMC cannot learn
// them out-of-band), this BMC is itself the authority for these devices: it
// authors the LUN backing files. That also keeps the implementation strictly
// standard — Drives is a server-populated property array, not a
// client-creatable collection.
//
// lun.0 is the U-Boot boot image the managed Pi boots from; lun.1 is the
// operator's virtual-media ISO and appears only while inserted (the same
// state VirtualMedia reports).
//
// A second subsystem, "Host", carries the drives the host itself probed:
// U-Boot walks its block devices after "nvme scan; usb start" and writes
// the list to the blkinfo EEPROM region (SMBIOS has no disk structure), so
// the BMC can report them while the host is off. That list is the host's
// honest bus view — the BMC's own gadget LUNs appear there too, as the USB
// drives the host sees them as; the two subsystems describe the same
// devices from either end of the cable.

import (
	"fmt"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/server/service/blkinfo"
	"github.com/pi-bmc/nanokvm-app/server/service/firmware"
)

const (
	storageID        = "1"
	hostStorageID    = "Host"
	driveBootImageID = "BootImage"
	driveMediaID     = "VirtualMedia"
)

// Storage is the Redfish Storage resource (DSP2046 §6.87).
type Storage struct {
	Resource
	Status      *Status `json:"Status,omitempty"`
	Drives      Links   `json:"Drives"`
	DrivesCount int     `json:"Drives@odata.count"`
}

// Drive is the Redfish Drive resource (DSP2046 §6.20).
type Drive struct {
	Resource
	Model         string             `json:"Model,omitempty"`
	CapacityBytes *int64             `json:"CapacityBytes,omitempty"`
	MediaType     schemas.MediaType  `json:"MediaType,omitempty"`
	Protocol      schemas.Protocol   `json:"Protocol,omitempty"`
	Status        *Status            `json:"Status,omitempty"`
}

// gadgetDrives returns the currently backed LUNs as (id, resource) pairs in
// stable order.
func gadgetDrives() []Drive {
	var drives []Drive
	fw := firmware.GetController()

	if st := fw.GetStatus(); st.ImagePath != "" {
		if fi, err := os.Stat(st.ImagePath); err == nil && fi.Size() > 0 {
			drives = append(drives, driveResource(driveBootImageID,
				"USB gadget LUN 0 (host boot image)", fi.Size()))
		}
	}
	if vm := fw.GetVirtualMediaState(); vm.Inserted {
		name := vm.ImageName
		if name == "" {
			name = "virtual media"
		}
		drives = append(drives, driveResource(driveMediaID,
			"USB gadget LUN 1 ("+name+")", vm.ImageSize))
	}
	return drives
}

func driveResource(id, model string, size int64) Drive {
	d := Drive{
		Resource: Resource{
			ODataType:    "#Drive.v1_17_0.Drive",
			ODataID:      drivesPath + "/" + id,
			ODataContext: context("Drive.Drive"),
			ID:           id,
			Name:         model,
		},
		Model: model,
		// The host sees a removable USB mass-storage device backed by
		// flash; SSD is the closest MediaType (there is no "USB" value).
		MediaType: schemas.SSDMediaType,
		Protocol:  schemas.USBProtocol,
		Status:    &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	}
	if size > 0 {
		d.CapacityBytes = &size
	}
	return d
}

// hostDriveProtocol maps a U-Boot interface name onto the Redfish Protocol
// enum; unknown interfaces are omitted rather than guessed.
func hostDriveProtocol(iface string) schemas.Protocol {
	switch iface {
	case "nvme":
		return schemas.NVMeProtocol
	case "usb":
		return schemas.USBProtocol
	case "mmc":
		// The closest value the enum has; there is no "SD".
		return schemas.EMMCProtocol
	case "sata", "scsi", "ide":
		return schemas.SATAProtocol
	default:
		return ""
	}
}

// hostDrives returns the drives the host's U-Boot reported through the
// blkinfo EEPROM region, or nil when the region is unreadable (the host has
// not booted a blkinfo-capable firmware yet).
func hostDrives() []Drive {
	inv, err := blkinfo.GetStore().Load()
	if err != nil || inv == nil {
		return nil
	}
	drives := make([]Drive, 0, len(inv.Drives))
	for _, hd := range inv.Drives {
		id := fmt.Sprintf("%s%d", hd.Interface, hd.Devnum)
		name := hd.Product
		if name == "" {
			name = id
		}
		d := Drive{
			Resource: Resource{
				ODataType:    "#Drive.v1_17_0.Drive",
				ODataID:      hostDrivesPath + "/" + id,
				ODataContext: context("Drive.Drive"),
				ID:           id,
				Name:         name,
			},
			Model:    hd.Product,
			Protocol: hostDriveProtocol(hd.Interface),
			// Boot-time snapshot: the drive was present when the host last
			// booted; the BMC cannot see hot-plug afterwards.
			Status: &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		}
		if hd.Interface == "nvme" {
			d.MediaType = schemas.SSDMediaType
		}
		if hd.SizeBytes > 0 && hd.SizeBytes <= uint64(1)<<62 {
			size := int64(hd.SizeBytes) //nolint:gosec // bounded above
			d.CapacityBytes = &size
		}
		drives = append(drives, d)
	}
	return drives
}

// storageSubsystem assembles one Storage resource from its drive list.
func storageSubsystem(id, path, name string, drives []Drive) Storage {
	links := make(Links, 0, len(drives))
	for _, d := range drives {
		links = append(links, Link(d.ODataID))
	}
	return Storage{
		Resource: Resource{
			ODataType:    "#Storage.v1_15_0.Storage",
			ODataID:      path,
			ODataContext: context("Storage.Storage"),
			ID:           id,
			Name:         name,
		},
		Status:      &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		Drives:      links,
		DrivesCount: len(links),
	}
}

func (s *Service) GetStorageCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"StorageCollection", "Storage Collection", storageRootPath,
		Link(storagePath), Link(hostStoragePath)))
}

func (s *Service) GetStorage(c *gin.Context) {
	switch c.Param("storage") {
	case storageID:
		c.JSON(http.StatusOK, storageSubsystem(storageID, storagePath,
			"USB Mass Storage (BMC gadget)", gadgetDrives()))
	case hostStorageID:
		c.JSON(http.StatusOK, storageSubsystem(hostStorageID, hostStoragePath,
			"Host Storage (U-Boot probe)", hostDrives()))
	default:
		redfishErrorResponse(c, http.StatusNotFound, "storage subsystem not found")
	}
}

func (s *Service) GetDrive(c *gin.Context) {
	var drives []Drive
	switch c.Param("storage") {
	case storageID:
		drives = gadgetDrives()
	case hostStorageID:
		drives = hostDrives()
	default:
		redfishErrorResponse(c, http.StatusNotFound, "storage subsystem not found")
		return
	}
	want := c.Param("drive")
	for _, d := range drives {
		if d.ID == want {
			c.JSON(http.StatusOK, d)
			return
		}
	}
	redfishErrorResponse(c, http.StatusNotFound, "drive not found: "+want)
}
