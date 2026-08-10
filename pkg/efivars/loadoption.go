package efivars

// loadoption.go parses EFI_LOAD_OPTION values (Boot#### variables) and
// classifies their device paths into Redfish boot source targets.

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// LoadOptionActive is the EFI_LOAD_OPTION attribute bit for enabled entries.
const LoadOptionActive = 0x00000001

// BootTarget is a coarse classification of where a boot option boots from,
// aligned with Redfish BootSourceOverrideTarget values.
type BootTarget string

const (
	TargetUnknown  BootTarget = ""
	TargetPxe      BootTarget = "Pxe"
	TargetHdd      BootTarget = "Hdd"
	TargetCd       BootTarget = "Cd"
	TargetUefiHttp BootTarget = "UefiHttp"
)

// LoadOption is a parsed EFI_LOAD_OPTION (a Boot#### variable value).
type LoadOption struct {
	Attributes  uint32
	Description string
	// DevicePath is the raw packed device path list.
	DevicePath []byte
	// OptionalData is everything after the device path list.
	OptionalData []byte
}

// Active reports whether the boot entry is enabled.
func (o *LoadOption) Active() bool {
	return o.Attributes&LoadOptionActive != 0
}

// ParseLoadOption decodes an EFI_LOAD_OPTION:
//
//	u32  Attributes
//	u16  FilePathListLength
//	u16  Description[]      — UTF-16LE, NUL-terminated
//	u8   FilePathList[FilePathListLength]
//	u8   OptionalData[]
func ParseLoadOption(b []byte) (*LoadOption, error) {
	if len(b) < 6 {
		return nil, fmt.Errorf("efivars: load option too short (%d bytes)", len(b))
	}
	o := &LoadOption{Attributes: binary.LittleEndian.Uint32(b[0:4])}
	pathLen := int(binary.LittleEndian.Uint16(b[4:6]))

	var units []uint16
	off := 6
	for {
		if off+2 > len(b) {
			return nil, fmt.Errorf("efivars: unterminated load option description")
		}
		u := binary.LittleEndian.Uint16(b[off : off+2])
		off += 2
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	o.Description = string(utf16.Decode(units))

	if off+pathLen > len(b) {
		return nil, fmt.Errorf("efivars: load option device path exceeds value")
	}
	o.DevicePath = append([]byte(nil), b[off:off+pathLen]...)
	o.OptionalData = append([]byte(nil), b[off+pathLen:]...)
	return o, nil
}

// Device path node types/subtypes (UEFI spec ch. 10).
const (
	dpTypeMessaging = 0x03
	dpTypeMedia     = 0x04

	dpMsgUSB  = 0x05
	dpMsgMAC  = 0x0b
	dpMsgIPv4 = 0x0c
	dpMsgIPv6 = 0x0d
	dpMsgSATA = 0x12
	dpMsgNVMe = 0x17
	dpMsgURI  = 0x18
	dpMsgSD   = 0x1a
	dpMsgEMMC = 0x1d

	dpMediaHardDrive = 0x01
	dpMediaCDROM     = 0x02
)

// Target classifies the load option's device path. Priority: network
// identities beat media nodes so a MAC()/Uri() option is never mistaken
// for a disk entry, and CDROM/USB (virtual media) beats plain HD.
func (o *LoadOption) Target() BootTarget {
	var hasMAC, hasURI, hasCD, hasUSB, hasDisk bool

	forEachDevicePathNode(o.DevicePath, func(typ, sub byte) {
		switch typ {
		case dpTypeMessaging:
			switch sub {
			case dpMsgURI:
				hasURI = true
			case dpMsgMAC, dpMsgIPv4, dpMsgIPv6:
				hasMAC = true
			case dpMsgUSB:
				hasUSB = true
			case dpMsgSATA, dpMsgNVMe, dpMsgSD, dpMsgEMMC:
				hasDisk = true
			}
		case dpTypeMedia:
			switch sub {
			case dpMediaCDROM:
				hasCD = true
			case dpMediaHardDrive:
				hasDisk = true
			}
		}
	})

	switch {
	case hasURI:
		return TargetUefiHttp
	case hasMAC:
		return TargetPxe
	case hasCD, hasUSB:
		// USB counts as removable media: the BMC presents virtual media
		// (and the firmware image) as a USB mass-storage gadget.
		return TargetCd
	case hasDisk:
		return TargetHdd
	default:
		return TargetUnknown
	}
}

// forEachDevicePathNode walks the packed node list, stopping at the
// end-of-path node or any malformed length.
func forEachDevicePathNode(b []byte, fn func(typ, sub byte)) {
	off := 0
	for off+4 <= len(b) {
		typ := b[off]
		sub := b[off+1]
		length := int(binary.LittleEndian.Uint16(b[off+2 : off+4]))
		if length < 4 || off+length > len(b) {
			return
		}
		if typ == 0x7f { // end of device path
			return
		}
		fn(typ, sub)
		off += length
	}
}

// EncodeLoadOption serializes a LoadOption back to EFI_LOAD_OPTION bytes.
func EncodeLoadOption(o *LoadOption) []byte {
	desc := utf16.Encode([]rune(o.Description))
	b := make([]byte, 6+2*(len(desc)+1)+len(o.DevicePath)+len(o.OptionalData))
	binary.LittleEndian.PutUint32(b[0:4], o.Attributes)
	binary.LittleEndian.PutUint16(b[4:6], uint16(len(o.DevicePath))) //nolint:gosec // spec-bounded
	off := 6
	for _, u := range desc {
		binary.LittleEndian.PutUint16(b[off:off+2], u)
		off += 2
	}
	off += 2 // NUL
	off += copy(b[off:], o.DevicePath)
	copy(b[off:], o.OptionalData)
	return b
}
