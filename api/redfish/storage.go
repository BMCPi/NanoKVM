package redfish

// storage.go exposes two storage subsystems:
//
//   - "1" is the managed host's own storage, populated by the host firmware's
//     drive reports over the host interface (hostreports.go). The BMC cannot
//     probe the host's buses; a Drive here is exactly what the host said it
//     has, readable even while the host is off.
//   - "BMC" is the storage the BMC itself presents to the host — the USB
//     mass-storage gadget's LUNs, one Drive per backed LUN. The BMC is the
//     authority for these: it authors the LUN backing files. lun.0 is the FMP
//     capsule volume the host's firmware scans at boot; lun.1 is the
//     operator's virtual-media ISO and appears only while inserted.
//
// The two subsystems describe the same cable from either end: the gadget
// LUNs show up again in "1" as the USB drives the host sees them as.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/app/firmware"
)

const (
	storageID       = "1"
	gadgetStorageID = "BMC"
	driveCapsuleID  = "CapsuleVolume"
	driveMediaID    = "VirtualMedia"
)

// Storage is the Redfish Storage resource (DSP2046 §6.87).
type Storage struct {
	Resource
	Status      *Status `json:"Status,omitempty"`
	Drives      Links   `json:"Drives"`
	DrivesCount int     `json:"Drives@odata.count"`
}

// Drive is the Redfish Drive resource (DSP2046 §6.20), used for the
// BMC-owned gadget LUNs. Host-reported drives are stored and served as raw
// maps (hostreports.go).
type Drive struct {
	Resource
	Manufacturer  string            `json:"Manufacturer,omitempty"`
	Model         string            `json:"Model,omitempty"`
	SerialNumber  string            `json:"SerialNumber,omitempty"`
	Revision      string            `json:"Revision,omitempty"`
	CapacityBytes *int64            `json:"CapacityBytes,omitempty"`
	MediaType     schemas.MediaType `json:"MediaType,omitempty"`
	Protocol      schemas.Protocol  `json:"Protocol,omitempty"`
	Status        *Status           `json:"Status,omitempty"`
}

// gadgetDrives returns the currently backed LUNs as (id, resource) pairs in
// stable order.
func gadgetDrives(fw *firmware.Controller) []Drive {
	var drives []Drive

	if st := fw.GetStatus(); st.VolumeReady {
		drives = append(drives, driveResource(driveCapsuleID,
			"USB gadget LUN 0 (FMP capsule volume)", st.VolumeSize))
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
			ODataID:      gadgetDrivesPath + "/" + id,
			ODataContext: odataContext("Drive.Drive"),
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

// storageSubsystem assembles one Storage resource from its drive links.
func storageSubsystem(id, path, name string, driveLinks Links) Storage {
	return Storage{
		Resource: Resource{
			ODataType:    "#Storage.v1_15_0.Storage",
			ODataID:      path,
			ODataContext: odataContext("Storage.Storage"),
			ID:           id,
			Name:         name,
		},
		Status:      &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		Drives:      driveLinks,
		DrivesCount: len(driveLinks),
	}
}

// hostStorageSubsystem is subsystem "1", linking the host-reported drives.
func hostStorageSubsystem() Storage {
	ids := hostCollectionIDs(drivesOf)
	links := make(Links, 0, len(ids))
	for _, id := range ids {
		links = append(links, Link(drivesPath+"/"+id))
	}
	return storageSubsystem(storageID, storagePath, "Host Storage", links)
}

func (s *Service) GetStorageCollection(c *gin.Context) {
	writeHostResource(c, hostView(newCollection(
		"StorageCollection", "Storage Collection", storageRootPath,
		Link(storagePath), Link(gadgetStoragePath))))
}

func (s *Service) GetStorage(c *gin.Context) {
	switch c.Param("storage") {
	case storageID:
		writeHostResource(c, hostView(hostStorageSubsystem()))
	case gadgetStorageID:
		drives := gadgetDrives(s.Firmware)
		links := make(Links, 0, len(drives))
		for _, d := range drives {
			links = append(links, Link(d.ODataID))
		}
		c.JSON(http.StatusOK, storageSubsystem(gadgetStorageID, gadgetStoragePath,
			"USB Mass Storage (BMC gadget)", links))
	default:
		redfishErrorResponse(c, http.StatusNotFound, "storage subsystem not found")
	}
}

func (s *Service) GetDrive(c *gin.Context) {
	want := c.Param("drive")
	switch c.Param("storage") {
	case storageID:
		stored, ok := hostCollectionGet(drivesOf, want)
		if !ok {
			redfishErrorResponse(c, http.StatusNotFound, "drive not found: "+want)
			return
		}
		writeHostResource(c, renderHostMember(stored, drivesPath+"/"+want, want,
			"#Drive.v1_17_0.Drive", "Drive.Drive", want))
	case gadgetStorageID:
		for _, d := range gadgetDrives(s.Firmware) {
			if d.ID == want {
				c.JSON(http.StatusOK, d)
				return
			}
		}
		redfishErrorResponse(c, http.StatusNotFound, "drive not found: "+want)
	default:
		redfishErrorResponse(c, http.StatusNotFound, "storage subsystem not found")
	}
}
