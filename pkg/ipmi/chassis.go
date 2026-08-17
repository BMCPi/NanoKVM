package ipmi

import (
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
)

// IPMI boot device selector byte values (bits 5:2 of boot flags byte 2,
// per IPMI 2.0 Table 28-14), mapped straight onto the Redfish
// BootSourceOverrideTarget the host firmware consumes.
const (
	ipmiBootDevNone        byte = 0x00 // no override
	ipmiBootDevPXE         byte = 0x04 // force PXE
	ipmiBootDevDisk        byte = 0x08 // force default hard disk
	ipmiBootDevCDROM       byte = 0x14 // force CD/DVD (virtual media)
	ipmiBootDevBIOS        byte = 0x18 // force BIOS/UEFI setup
	ipmiBootDevHTTP        byte = 0x20 // force boot from remote media (UEFI HTTP)
	ipmiBootDevPrimaryDisk byte = 0x24 // force primary hard disk
)

// ipmiDeviceToTarget maps the selector onto a Redfish target string.
var ipmiDeviceToTarget = map[byte]string{
	ipmiBootDevNone:        "None",
	ipmiBootDevPXE:         "Pxe",
	ipmiBootDevDisk:        "Hdd",
	ipmiBootDevCDROM:       "Cd",
	ipmiBootDevBIOS:        "BiosSetup",
	ipmiBootDevHTTP:        "UefiHttp",
	ipmiBootDevPrimaryDisk: "Hdd",
}

// targetToIPMIDevice maps a Redfish target back onto the selector.
func targetToIPMIDevice(target string) (byte, bool) {
	switch target {
	case "Pxe":
		return ipmiBootDevPXE, true
	case "Hdd":
		return ipmiBootDevDisk, true
	case "Cd":
		return ipmiBootDevCDROM, true
	case "BiosSetup":
		return ipmiBootDevBIOS, true
	case "UefiHttp":
		return ipmiBootDevHTTP, true
	default:
		return ipmiBootDevNone, false
	}
}

// handleGetDeviceID returns BMC device identification per IPMI Table 20-2.
func handleGetDeviceID() []byte {
	resp := make([]byte, 16)
	resp[0] = ccOK
	resp[1] = 0x20 // Device ID
	resp[2] = 0x01 // Device Revision
	resp[3] = 0x02 // Firmware Revision 1 (major): 2
	resp[4] = 0x00 // Firmware Revision 2 (minor): 0
	resp[5] = 0x02 // IPMI version: 2.0
	resp[6] = 0x2F // Additional device support (chassis, SEL, SDR, FRU, IPMB)
	resp[7] = 0xA2 // Manufacturer ID (3 bytes, LE) — placeholder
	resp[8] = 0x02
	resp[9] = 0x00
	resp[10] = 0x01 // Product ID (2 bytes, LE)
	resp[11] = 0x00
	resp[12] = 0x00 // Aux Firmware Revision
	resp[13] = 0x00
	resp[14] = 0x00
	resp[15] = 0x00
	return resp
}

// handleGetChassisStatus reads the power state via the injected controller.
func (sm *sessionManager) handleGetChassisStatus() []byte {
	powerOn := false
	on, err := sm.power.State()
	if err != nil {
		log.Errorf("IPMI: failed to read power state: %s", err)
	} else {
		powerOn = on
	}

	// Chassis Status response: completion code + 3 mandatory bytes
	resp := make([]byte, 4)
	resp[0] = ccOK
	if powerOn {
		resp[1] = 0x01 // system power is on
	}
	resp[2] = 0x00 // last power event: unknown
	resp[3] = 0x00 // misc: nothing special
	return resp
}

// handleChassisControl executes power/reset operations via the injected controller.
func (sm *sessionManager) handleChassisControl(cmdData []byte) []byte {
	if len(cmdData) < 1 {
		return []byte{ccInvalidParam}
	}

	action := cmdData[0] & 0x0F
	ctrl := sm.power

	switch action {
	case controlPowerUp:
		log.Info("IPMI: chassis power on")
		go func() {
			if err := ctrl.PowerOn(); err != nil {
				log.Errorf("IPMI: power on failed: %s", err)
			}
		}()

	case controlPowerDown:
		log.Info("IPMI: chassis power off")
		go func() {
			if err := ctrl.PowerOff(); err != nil {
				log.Errorf("IPMI: power off failed: %s", err)
			}
		}()

	case controlPowerCycle:
		log.Info("IPMI: chassis power cycle")
		go func() {
			if err := ctrl.Reset(); err != nil {
				log.Errorf("IPMI: power cycle failed: %s", err)
			}
		}()

	case controlHardReset:
		log.Info("IPMI: chassis hard reset")
		go func() {
			if err := ctrl.Reset(); err != nil {
				log.Errorf("IPMI: reset failed: %s", err)
			}
		}()

	case controlSoftShutdown:
		log.Info("IPMI: chassis soft shutdown")
		go func() {
			if err := ctrl.PowerOff(); err != nil {
				log.Errorf("IPMI: soft shutdown failed: %s", err)
			}
		}()

	default:
		log.Debugf("IPMI: unsupported chassis control action: 0x%02x", action)
		return []byte{ccInvalidParam}
	}

	return []byte{ccOK}
}

// handleSetSystemBootOptions stages the boot override in the BMC's Redfish
// state; the host's firmware reads and applies it over the host interface.
func (sm *sessionManager) handleSetSystemBootOptions(cmdData []byte) []byte {
	if len(cmdData) < 1 {
		return []byte{ccInvalidParam}
	}

	paramSelector := cmdData[0] & 0x7F

	switch paramSelector {
	case bootParamSetInProgress:
		return []byte{ccOK}

	case bootParamBootFlags:
		if len(cmdData) < 6 {
			return []byte{ccInvalidParam}
		}

		valid := cmdData[1]&0x80 != 0
		// Bit 6: 0 = apply to next boot only (once), 1 = apply to all future boots (persistent).
		persistent := cmdData[1]&0x40 != 0
		device := cmdData[2] & 0x3C // extract bits 5:2

		log.Debugf("IPMI: set boot device=0x%02x valid=%v persistent=%v", device, valid, persistent)

		target, known := ipmiDeviceToTarget[device]
		if !valid || !known || target == "None" {
			redfish.SetBootOverride("None", "Disabled")
			return []byte{ccOK}
		}

		enabled := "Once"
		if persistent {
			enabled = "Continuous"
		}
		redfish.SetBootOverride(target, enabled)
		return []byte{ccOK}

	default:
		return []byte{ccOK}
	}
}

// handleGetSystemBootOptions reads the staged boot override back out of the
// Redfish state.
func (sm *sessionManager) handleGetSystemBootOptions(cmdData []byte) []byte {
	if len(cmdData) < 1 {
		return []byte{ccInvalidParam}
	}

	paramSelector := cmdData[0] & 0x7F

	switch paramSelector {
	case bootParamBootFlags:
		override := redfish.BootOverride()
		dev, known := targetToIPMIDevice(override.Target)
		valid := known && override.Enabled != "Disabled" && override.Enabled != ""
		persistent := valid && override.Enabled == "Continuous"
		if !valid {
			dev = ipmiBootDevNone
		}

		resp := make([]byte, 8)
		resp[0] = ccOK
		resp[1] = 0x01 // parameter revision
		resp[2] = bootParamBootFlags
		flags := byte(0)
		if valid {
			flags |= 0x80 // validity bit
		}
		if persistent {
			flags |= 0x40 // persistent bit
		}
		resp[3] = flags
		resp[4] = dev
		return resp

	default:
		resp := make([]byte, 3)
		resp[0] = ccOK
		resp[1] = 0x01
		resp[2] = paramSelector
		return resp
	}
}
