package config

import (
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
)

type HWVersion int

const (
	HWVersionAlpha HWVersion = iota
	HWVersionBeta
	HWVersionPcie

	HWVersionFile = "/etc/kvm/hw"
)

// GPIO character-device (CONFIG_GPIO_CDEV) addressing.
//
// The power/reset/LED lines all live on a single 32-line bank of the
// SG2002/CV1800B. Historically they were reached through the deprecated sysfs
// numbering as global GPIOs 503/504/505/507. Under the character-device
// interface a line is addressed by (gpiochip, offset) instead, where
//
//	offset = sysfsGlobalNumber - chipBase
//
// powerChip / powerChipBase capture that single bank so the mapping lives in
// one place. VERIFY on the target with `gpiodetect` + `gpioinfo`: confirm which
// /dev/gpiochipN carries these lines and its base, then adjust the two
// constants below if they differ. Everything else is derived.
const (
	// powerChip is the gpiochip carrying the power/reset/LED lines.
	powerChip = "gpiochip0"
	// powerChipBase is the legacy sysfs base of powerChip (dynamic top-down
	// allocation: ARCH_NR_GPIOS(512) - 32 lines = 480, so gpio503 -> offset 23).
	powerChipBase = 480
)

// pin maps a legacy global sysfs GPIO number to its cdev (chip, offset).
func pin(sysfsGlobal int) GPIOPin {
	return GPIOPin{Chip: powerChip, Line: sysfsGlobal - powerChipBase}
}

var HWAlpha = Hardware{
	Version:      HWVersionAlpha,
	GPIOReset:    pin(507),
	GPIOPower:    pin(503),
	GPIOPowerLED: pin(504),
	GPIOHDDLed:   pin(505),
}

var HWBeta = Hardware{
	Version:      HWVersionBeta,
	GPIOReset:    pin(505),
	GPIOPower:    pin(503),
	GPIOPowerLED: pin(504),
	GPIOHDDLed:   GPIOPin{},
}

var HWPcie = Hardware{
	Version:      HWVersionPcie,
	GPIOReset:    pin(505),
	GPIOPower:    pin(503),
	GPIOPowerLED: pin(504),
	GPIOHDDLed:   GPIOPin{},
}

func (h HWVersion) String() string {
	switch h {
	case HWVersionAlpha:
		return "Alpha"
	case HWVersionBeta:
		return "Beta"
	case HWVersionPcie:
		return "PCIE"
	default:
		return "Unknown"
	}
}

func GetHwVersion() HWVersion {
	content, err := os.ReadFile(HWVersionFile)
	if err != nil {
		return HWVersionAlpha
	}

	version := strings.ReplaceAll(string(content), "\n", "")
	switch version {
	case "alpha":
		return HWVersionAlpha
	case "beta":
		return HWVersionBeta
	case "pcie":
		return HWVersionPcie
	default:
		return HWVersionAlpha
	}
}

func getHardware() (h Hardware) {
	version := GetHwVersion()

	switch version {
	case HWVersionAlpha:
		h = HWAlpha

	case HWVersionBeta:
		h = HWBeta

	case HWVersionPcie:
		h = HWPcie

	default:
		h = HWAlpha
		log.Errorf("Unsupported hardware version: %s", version)
	}

	return
}
