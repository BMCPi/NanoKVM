package redfish

import (
	"testing"

	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/server/service/blkinfo"
)

// NVMe: U-Boot's blk_desc carries the Identify model name in vendor and the
// serial in product; the Drive members must be assigned accordingly.
func TestHostDriveResourceNVMe(t *testing.T) {
	d := hostDriveResource(blkinfo.Drive{
		Interface: "nvme", Devnum: 0,
		Vendor:   "Samsung SSD 990 EVO 1TB",
		Product:  "S7M4NJ0X123456",
		Revision: "0B2QKXJ7", SizeBytes: 1000204886016,
	})
	if d.ID != "nvme0" || d.Model != "Samsung SSD 990 EVO 1TB" ||
		d.SerialNumber != "S7M4NJ0X123456" || d.Revision != "0B2QKXJ7" {
		t.Errorf("nvme mapping wrong: %+v", d)
	}
	if d.Manufacturer != "" {
		t.Errorf("nvme Manufacturer = %q, want empty (vendor field holds the model)", d.Manufacturer)
	}
	if d.Protocol != schemas.NVMeProtocol || d.MediaType != schemas.SSDMediaType {
		t.Errorf("nvme protocol/media wrong: %+v", d)
	}
	if d.Name != d.Model {
		t.Errorf("Name = %q, want the model", d.Name)
	}
	if d.CapacityBytes == nil || *d.CapacityBytes != 1000204886016 {
		t.Errorf("CapacityBytes = %v", d.CapacityBytes)
	}
}

// SCSI-shaped devices (USB mass storage) use vendor/product literally.
func TestHostDriveResourceUSB(t *testing.T) {
	d := hostDriveResource(blkinfo.Drive{
		Interface: "usb", Devnum: 0,
		Vendor: "Linux", Product: "File-Stor Gadget",
		Removable: 1, SizeBytes: 537936896,
	})
	if d.Manufacturer != "Linux" || d.Model != "File-Stor Gadget" || d.SerialNumber != "" {
		t.Errorf("usb mapping wrong: %+v", d)
	}
	if d.Protocol != schemas.USBProtocol || d.MediaType != "" {
		t.Errorf("usb protocol/media wrong: %+v", d)
	}
}

// A drive with no strings still gets a usable Name from its Id.
func TestHostDriveResourceNameFallback(t *testing.T) {
	d := hostDriveResource(blkinfo.Drive{Interface: "mmc", Devnum: 0})
	if d.Name != "mmc0" {
		t.Errorf("Name = %q, want mmc0", d.Name)
	}
}
