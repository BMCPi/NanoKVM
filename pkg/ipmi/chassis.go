package ipmi

import (
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

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

// handleSetSystemBootOptions stores boot device override in firmware env.
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

		// Persist to firmware env if available.
		fwCtrl := sm.firmware
		ubootTargets, ok := firmware.IPMIDeviceToUBoot[device]
		if !ok {
			ubootTargets = ""
		}

		if valid {
			var err error
			if persistent {
				err = fwCtrl.SetBootTarget(ubootTargets)
			} else {
				err = fwCtrl.SetBootTargetOnce(ubootTargets)
			}
			if err != nil {
				log.Warnf("IPMI: firmware env write failed: %v", err)
			}
		} else {
			// Clear both override files.
			if err := fwCtrl.SetBootTarget(""); err != nil {
				log.Warnf("IPMI: firmware env clear (persistent) failed: %v", err)
			}
			if err := fwCtrl.SetBootTargetOnce(""); err != nil {
				log.Warnf("IPMI: firmware env clear (once) failed: %v", err)
			}
		}

		return []byte{ccOK}

	default:
		return []byte{ccOK}
	}
}

// handleGetSystemBootOptions returns the stored boot device override.
func (sm *sessionManager) handleGetSystemBootOptions(cmdData []byte) []byte {
	if len(cmdData) < 1 {
		return []byte{ccInvalidParam}
	}

	paramSelector := cmdData[0] & 0x7F

	switch paramSelector {
	case bootParamBootFlags:
		// Read from firmware env — prefer persistent, fall back to once.
		fwCtrl := sm.firmware
		var dev byte
		var valid bool
		var persistent bool

		if ubootTargets, err := fwCtrl.GetBootTarget(); err == nil && ubootTargets != "" {
			if d, ok := firmware.UBootToIPMIDevice[ubootTargets]; ok {
				dev = d
				valid = true
				persistent = true
			}
		}
		if !valid {
			if ubootTargets, err := fwCtrl.GetOnceBootTarget(); err == nil && ubootTargets != "" {
				if d, ok := firmware.UBootToIPMIDevice[ubootTargets]; ok {
					dev = d
					valid = true
					persistent = false
				}
			}
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
