package ipmi

import (
	"context"
	"log/slog"
	"sync"

	"github.com/bougou/go-ipmi/pkg/hal"
	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

// chassisHAL adapts the GPIO power controller and the Redfish boot-override
// state to hal.ChassisHAL.
//
// Power actions are fired detached and report success immediately, exactly as
// the previous server did: a power sequence is seconds of GPIO choreography,
// and holding the UDP response (and the framework's dispatch goroutine) open
// for it would time the client out on an action that is in fact proceeding.
// root bounds those detached sequences at shutdown.
type chassisHAL struct {
	root  context.Context
	power powerController
	log   *slog.Logger

	mu sync.Mutex
	// bootFlags holds the last full structure a client set. The Redfish
	// override only models device + persistence, so the rest is kept here
	// and echoed back rather than silently dropped (the HAL contract).
	bootFlags *types.BootOptionParam_BootFlags
	bootAck   *types.BootOptionParam_BootInfoAcknowledge
}

// detach runs op with the action timeout, off the dispatch goroutine.
func (c *chassisHAL) detach(what string, op func(ctx context.Context) error) {
	c.log.Info("ipmi: chassis action", slog.String("action", what))
	go func() {
		ctx, cancel := context.WithTimeout(c.root, power.ActionTimeout)
		defer cancel()
		if err := op(ctx); err != nil {
			c.log.ErrorContext(ctx, "ipmi: chassis action failed",
				slog.String("action", what), slog.Any("err", err))
		}
	}()
}

func (c *chassisHAL) PowerState(ctx context.Context) (bool, error) {
	return c.power.State(ctx)
}

func (c *chassisHAL) SetPower(_ context.Context, on bool) error {
	if on {
		c.detach("power on", c.power.PowerOn)
	} else {
		c.detach("power off", c.power.PowerOff)
	}
	return nil
}

func (c *chassisHAL) PowerCycle(_ context.Context) error {
	c.detach("power cycle", c.power.Reset)
	return nil
}

func (c *chassisHAL) ColdReset(_ context.Context) error {
	c.detach("hard reset", c.power.Reset)
	return nil
}

// WarmReset backs Chassis Control "soft shutdown" in this framework, and the
// previous server answered that action with a graceful power-off (a power
// button tap the host OS handles). Kept, so `ipmitool power soft` still means
// what it always did on this BMC.
func (c *chassisHAL) WarmReset(_ context.Context) error {
	c.detach("soft shutdown", c.power.PowerOff)
	return nil
}

func (c *chassisHAL) Identify(_ context.Context, _ uint8) error {
	return hal.ErrNotSupported
}

func (c *chassisHAL) IntrusionState(_ context.Context) (bool, error) {
	return false, hal.ErrNotSupported
}

// IPMI boot device selectors (Table 28-14 bits 5:2) mapped onto the Redfish
// BootSourceOverrideTarget values the host firmware consumes. Same mapping
// the previous server used.
const (
	bootDevNone        types.BootDeviceSelector = 0x0
	bootDevPXE         types.BootDeviceSelector = 0x1
	bootDevDisk        types.BootDeviceSelector = 0x2
	bootDevCDROM       types.BootDeviceSelector = 0x5
	bootDevBIOS        types.BootDeviceSelector = 0x6
	bootDevHTTP        types.BootDeviceSelector = 0x8
	bootDevPrimaryDisk types.BootDeviceSelector = 0x9
)

var bootDevToTarget = map[types.BootDeviceSelector]string{
	bootDevNone:        "None",
	bootDevPXE:         "Pxe",
	bootDevDisk:        "Hdd",
	bootDevCDROM:       "Cd",
	bootDevBIOS:        "BiosSetup",
	bootDevHTTP:        "UefiHttp",
	bootDevPrimaryDisk: "Hdd",
}

func targetToBootDev(target string) (types.BootDeviceSelector, bool) {
	switch target {
	case "Pxe":
		return bootDevPXE, true
	case "Hdd":
		return bootDevDisk, true
	case "Cd":
		return bootDevCDROM, true
	case "BiosSetup":
		return bootDevBIOS, true
	case "UefiHttp":
		return bootDevHTTP, true
	default:
		return bootDevNone, false
	}
}

// SetBootFlags stages the override in the BMC's Redfish state — the host's
// firmware reads and applies it over the host interface — and keeps the full
// structure so GetBootFlags can echo fields Redfish does not model.
func (c *chassisHAL) SetBootFlags(_ context.Context, flags *types.BootOptionParam_BootFlags) error {
	stored := *flags
	c.mu.Lock()
	c.bootFlags = &stored
	c.mu.Unlock()

	target, known := bootDevToTarget[flags.BootDeviceSelector]
	if !flags.BootFlagsValid || !known || target == "None" {
		redfish.SetBootOverride("None", "Disabled")
		return nil
	}
	enabled := "Once"
	if flags.Persist {
		enabled = "Continuous"
	}
	redfish.SetBootOverride(target, enabled)
	return nil
}

// GetBootFlags reads the override back out of the Redfish state, which is
// authoritative: the host clears it there once consumed, and an operator can
// change it over Redfish between IPMI calls.
func (c *chassisHAL) GetBootFlags(_ context.Context) (*types.BootOptionParam_BootFlags, error) {
	flags := types.BootOptionParam_BootFlags{}
	c.mu.Lock()
	if c.bootFlags != nil {
		flags = *c.bootFlags
	}
	c.mu.Unlock()

	override := redfish.BootOverride()
	dev, known := targetToBootDev(override.Target)
	valid := known && override.Enabled != "Disabled" && override.Enabled != ""
	flags.BootFlagsValid = valid
	flags.Persist = valid && override.Enabled == "Continuous"
	if !valid {
		dev = bootDevNone
	}
	flags.BootDeviceSelector = dev
	return &flags, nil
}

func (c *chassisHAL) SetBootInfoAcknowledge(_ context.Context, ack *types.BootOptionParam_BootInfoAcknowledge) error {
	stored := *ack
	c.mu.Lock()
	c.bootAck = &stored
	c.mu.Unlock()
	return nil
}

func (c *chassisHAL) GetBootInfoAcknowledge(_ context.Context) (*types.BootOptionParam_BootInfoAcknowledge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bootAck == nil {
		return nil, hal.ErrNotSupported
	}
	ack := *c.bootAck
	return &ack, nil
}
