package firmware

// gadget.go bridges the firmware Controller to the usbgadget package, which is
// the sole owner of the USB gadget configfs (/sys/kernel/config/usb_gadget/g0).
// The Controller only tracks the higher-level "presented" state its capsule
// write cycle depends on; every raw configfs write — lun.0/lun.1 backing files,
// UDC bind/unbind, LUN creation — lives in usbgadget.

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	"github.com/pi-bmc/nanokvm-app/pkg/usbgadget"
)

// presentVolume presents the FMP capsule volume on the gadget's lun.0.
// Idempotent. Must be called with c.mu held.
//
// The image file is presented directly (not a loop device) as a writable
// removable disk: the host's firmware deletes each capsule from
// \EFI\UpdateCapsule\ once it has applied it. The BMC only ever touches the
// volume while it is unpresented (see withVolume), so the two never write
// through the same bytes at once.
func (c *Controller) presentVolume() error {
	if c.presented {
		return nil
	}
	if err := usbgadget.Get().PresentDisk(c.capsulePath); err != nil {
		return err
	}
	c.presented = true
	telemetry.FirmwarePresented(context.Background(), true)
	log.Infof("firmware: presented capsule volume %s via USB gadget", c.capsulePath)
	return nil
}

// unpresentVolume clears the capsule volume from lun.0. After this returns the
// backing file is no longer held by f_mass_storage and is safe to rewrite.
// Must be called with c.mu held.
func (c *Controller) unpresentVolume() error {
	if !c.presented {
		return nil
	}
	if err := usbgadget.Get().UnpresentDisk(); err != nil {
		return err
	}
	c.presented = false
	telemetry.FirmwarePresented(context.Background(), false)
	log.Info("firmware: unpresented USB gadget")
	return nil
}

// Present presents the capsule volume via USB gadget (public, acquires lock).
func (c *Controller) Present() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presentVolume()
}

// Unpresent removes the capsule volume from the USB gadget (public, acquires
// lock). The host sees an empty drive until Present is called again.
func (c *Controller) Unpresent() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unpresentVolume()
}
