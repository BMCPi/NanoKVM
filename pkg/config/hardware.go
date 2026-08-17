package config

import (
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/warthog618/go-gpiocdev"
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
// The power/reset/LED lines all live on one 32-line bank of the SG2002/CV1800B
// (XGPIOA, gpio@3020000 — "porta" in the board device tree). They are resolved
// by the names that device tree gives them rather than by (gpiochipN, offset),
// because /dev/gpiochipN is numbered in controller *registration* order: adding
// any unrelated GPIO node renumbers every chip behind it.
//
// That is not a hypothetical. Declaring the RTC-domain bank (gpio@5021000) to
// reach the LT6911 bridge's reset line put it ahead of gpio@3020000, so a
// hardcoded "gpiochip0" started addressing an unconnected pad: every button
// press went nowhere and the power LED read a permanent 0 (which in turn made
// PowerOff/ForceOff short-circuit as "already off"). A line name comes from the
// device tree that describes the board, so it cannot drift that way.
const (
	lineNamePower    = "power-button"
	lineNamePowerLED = "power-led"
	lineNameHDDLed   = "hdd-led"
	lineNameReset    = "reset-button"
)

// headerChipLabel identifies the bank above for device trees that do not name
// their lines. A gpiochip's label is its MMIO address, fixed by the SoC, so it
// is stable in the way the gpiochipN name is not.
const headerChipLabel = "3020000.gpio"

// noLine marks a line a board revision does not wire up.
const noLine = -1

// hwProfile is one board revision's line map: each line's offset within the
// header bank, or noLine. The offsets are only the fallback for a device tree
// without line names — they are the legacy sysfs numbers (503 power, 504
// power-LED, 505 HDD-LED, 507 reset) less that bank's old sysfs base of 480.
type hwProfile struct {
	version                        HWVersion
	power, powerLED, hddLED, reset int
}

var (
	profileAlpha = hwProfile{version: HWVersionAlpha, power: 23, powerLED: 24, hddLED: 25, reset: 27}
	profileBeta  = hwProfile{version: HWVersionBeta, power: 23, powerLED: 24, hddLED: noLine, reset: 25}
	profilePcie  = hwProfile{version: HWVersionPcie, power: 23, powerLED: 24, hddLED: noLine, reset: 25}
)

// hardware resolves the profile's lines against the running kernel.
func (p hwProfile) hardware() Hardware {
	var r lineResolver
	return Hardware{
		Version:      p.version,
		GPIOPower:    r.resolve(lineNamePower, p.power),
		GPIOPowerLED: r.resolve(lineNamePowerLED, p.powerLED),
		GPIOHDDLed:   r.resolve(lineNameHDDLed, p.hddLED),
		GPIOReset:    r.resolve(lineNameReset, p.reset),
	}
}

// lineResolver turns line names into pins. It caches the fallback chip lookup so
// /dev is scanned once per config load rather than once per line, and reports an
// unresolvable bank once instead of for every line on it.
type lineResolver struct {
	chip     string
	chipErr  error
	looked   bool
	reported bool
}

// resolve locates one line, preferring the device tree's name for it and falling
// back to the profile's offset on the label-matched bank. An unresolvable line
// comes back unset: refusing to drive a pin we cannot identify is the whole
// point, since driving the wrong one is silent and the symptoms are remote.
func (r *lineResolver) resolve(name string, offset int) GPIOPin {
	if offset == noLine {
		return GPIOPin{}
	}

	if chip, line, err := gpiocdev.FindLine(name); err == nil {
		log.Debugf("config: gpio %q -> %s:%d (named by the device tree)", name, chip, line)
		return GPIOPin{Chip: chip, Line: line}
	}

	chip, err := r.headerChip()
	if err != nil {
		if !r.reported {
			r.reported = true
			log.Errorf("config: no gpio lines resolvable: none are named in the device tree and %s — power control is unavailable", err)
		}
		return GPIOPin{}
	}

	log.Warnf("config: gpio line %q is not named in the device tree; falling back to %s:%d", name, chip, offset)
	return GPIOPin{Chip: chip, Line: offset}
}

func (r *lineResolver) headerChip() (string, error) {
	if !r.looked {
		r.looked = true
		r.chip, r.chipErr = findChipByLabel(headerChipLabel)
	}
	return r.chip, r.chipErr
}

// findChipByLabel returns the /dev/gpiochipN name of the chip carrying label.
func findChipByLabel(label string) (string, error) {
	chips := gpiocdev.Chips()
	if len(chips) == 0 {
		return "", fmt.Errorf("this kernel exposes no gpio character devices")
	}

	for _, name := range chips {
		c, err := gpiocdev.NewChip(name)
		if err != nil {
			continue
		}
		match := c.Label == label
		_ = c.Close()
		if match {
			return name, nil
		}
	}
	return "", fmt.Errorf("no gpiochip is labelled %q (found %s)", label, strings.Join(chips, ", "))
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

func getHardware() Hardware {
	version := GetHwVersion()

	switch version {
	case HWVersionAlpha:
		return profileAlpha.hardware()

	case HWVersionBeta:
		return profileBeta.hardware()

	case HWVersionPcie:
		return profilePcie.hardware()

	default:
		log.Errorf("Unsupported hardware version: %s", version)
		return profileAlpha.hardware()
	}
}
