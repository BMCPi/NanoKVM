package firmware

// gadget.go bridges the firmware Controller to the usbgadget package, which is
// the sole owner of the USB gadget configfs (/sys/kernel/config/usb_gadget/g0).
// The Controller only tracks the higher-level "presented" state that its mount
// cycle depends on; every raw configfs write — lun.0/lun.1 backing files, UDC
// bind/unbind, LUN creation — now lives in usbgadget.

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	"github.com/pi-bmc/nanokvm-app/pkg/usbgadget"
)

// presentImage presents the firmware image on the gadget's lun.0. Idempotent.
// Must be called with c.mu held.
//
// The image file is presented directly (not a loop device). The host (U-Boot)
// boots from this image. The BMC accesses the env partition only through
// short-lived mount/unmount cycles, avoiding dual-access conflicts with the
// gadget's file-backed I/O.
func (c *Controller) presentImage() error {
	if c.presented {
		return nil
	}
	if err := usbgadget.Get().PresentImage(c.imagePath); err != nil {
		return err
	}
	c.presented = true
	telemetry.FirmwarePresented(context.Background(), true)
	log.Infof("firmware: presented %s via USB gadget", c.imagePath)
	return nil
}

// unpresentImage clears the firmware image from lun.0. After this returns the
// backing file is no longer held by f_mass_storage and is safe to loop-mount.
// Must be called with c.mu held.
func (c *Controller) unpresentImage() error {
	if !c.presented {
		return nil
	}
	if err := usbgadget.Get().UnpresentImage(); err != nil {
		return err
	}
	c.presented = false
	telemetry.FirmwarePresented(context.Background(), false)
	log.Info("firmware: unpresented USB gadget")
	return nil
}

// Present presents the firmware image via USB gadget (public, acquires lock).
func (c *Controller) Present() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presentImage()
}

// Unpresent removes the firmware image from the USB gadget (public, acquires lock).
func (c *Controller) Unpresent() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unpresentImage()
}
