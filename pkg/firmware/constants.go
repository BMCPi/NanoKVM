package firmware

// Boot target constants — U-Boot env values for the boot_targets variable.
const (
	// BootTargetNone clears any boot override; U-Boot uses its default order.
	BootTargetNone = ""
	// BootTargetPxe forces network boot via DHCP/TFTP (PXE).
	BootTargetPxe = "dhcp"
	// BootTargetHdd forces boot from the default storage hierarchy (MMC, NVMe, USB).
	BootTargetHdd = "mmc nvme usb"
	// BootTargetVirtMedia forces boot from a usb-mapped virtual block device (ISO).
	BootTargetVirtMedia = "usb"
	// BootTargetUefiHttp forces UEFI HTTP boot.
	BootTargetUefiHttp = "httpboot"
)

// IPMI boot device selector byte values (bits 5:2 of boot flags byte 2,
// per IPMI 2.0 Table 28-14).
const (
	IPMIBootDevNone        byte = 0x00 // no override
	IPMIBootDevPXE         byte = 0x04 // force PXE
	IPMIBootDevDisk        byte = 0x08 // force default hard disk
	IPMIBootDevCDROM       byte = 0x14 // force CD/DVD (virtual media)
	IPMIBootDevBIOS        byte = 0x18 // force BIOS/UEFI setup
	IPMIBootDevHTTP        byte = 0x20 // force boot from primary remote media (UEFI HTTP)
	IPMIBootDevPrimaryDisk byte = 0x24 // force primary hard disk
)

// RedfishToUBoot maps Redfish BootSourceOverrideTarget values to U-Boot boot_targets.
var RedfishToUBoot = map[string]string{
	"None":      BootTargetNone,
	"Pxe":       BootTargetPxe,
	"Hdd":       BootTargetHdd,
	"Cd":        BootTargetVirtMedia,
	"BiosSetup": BootTargetNone,
	"UefiHttp":  BootTargetUefiHttp,
}

// UBootToRedfish maps U-Boot boot_targets back to Redfish BootSourceOverrideTarget.
var UBootToRedfish = map[string]string{
	BootTargetNone:      "None",
	BootTargetPxe:       "Pxe",
	BootTargetHdd:       "Hdd",
	BootTargetVirtMedia: "Cd",
	BootTargetUefiHttp:  "UefiHttp",
}

// IPMIDeviceToUBoot maps IPMI boot device selector bytes to U-Boot boot_targets.
var IPMIDeviceToUBoot = map[byte]string{
	IPMIBootDevNone:        BootTargetNone,
	IPMIBootDevPXE:         BootTargetPxe,
	IPMIBootDevDisk:        BootTargetHdd,
	IPMIBootDevCDROM:       BootTargetVirtMedia,
	IPMIBootDevBIOS:        BootTargetNone,
	IPMIBootDevHTTP:        BootTargetUefiHttp,
	IPMIBootDevPrimaryDisk: BootTargetHdd,
}

// UBootToIPMIDevice maps U-Boot boot_targets to IPMI boot device selector bytes.
var UBootToIPMIDevice = map[string]byte{
	BootTargetNone:      IPMIBootDevNone,
	BootTargetPxe:       IPMIBootDevPXE,
	BootTargetHdd:       IPMIBootDevDisk,
	BootTargetVirtMedia: IPMIBootDevCDROM,
	BootTargetUefiHttp:  IPMIBootDevHTTP,
}
