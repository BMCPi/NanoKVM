// pta.go is the normal-world client for the OP-TEE BMC sensor pseudo-TA.
//
// The pTA owns the I2C push to the BMC autonomously inside OP-TEE; nothing
// here is on that path, and nothing here runs on the BMC. This client does
// the two things the secure side cannot do for itself: re-run the RP1-BAR
// handshake when the OS has moved the BAR out from under the stored address,
// and observe the samples the pTA is publishing so the host can forward or
// log them. The BMC reads the same samples off its own emulated EEPROM
// instead — see pkg/bmcsensor.
//
// It is a port of docs/optee-sensor/bmc_sensord.c from the rpi5-uefi-build
// tree, over pkg/optee instead of libteec, so it cross-compiles with no cgo
// and no shared library on the target. The command ABI is the pTA's
// (optee-os files/plat-rpi5/pta_bmc_sensor.h) and is mirrored, not chosen —
// including the part where GET returns its sample in parameters 0 and 1
// while WAIT returns the same sample in parameters 1 and 2.
package main

import (
	"fmt"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/device/optee"
)

// UUIDString identifies the pTA (optee-os files/plat-rpi5/pta_bmc_sensor.h).
const UUIDString = "575d6607-5a2b-4384-827e-c16a25ac4fa5"

// TA commands.
const (
	cmdInit uint32 = 0
	cmdGet  uint32 = 1
	cmdWait uint32 = 2
)

// cancelID tags this client's blocking invokes so Cancel can name one. Any
// non-zero value works; the TEE only needs it to be distinguishable from the
// zero "not cancellable" id.
const cancelID uint32 = 1

// The pi-bmc wire contract. Only Init sends these: on the Raspberry Pi 5 EDK2
// has already run the handshake by the time Linux starts, so a plain Get or
// Wait never carries them.
const (
	// RP1BARDefault is where the RP1 southbridge BAR sits before an OS
	// reassigns it.
	RP1BARDefault uint64 = 0x1f00000000
	// RP1I2C1Offset is I2C1 within that BAR.
	RP1I2C1Offset uint32 = 0x74000
	// EEPROMSlave is the BMC's emulated EEPROM address on that bus.
	EEPROMSlave uint32 = 0x50
	// SensorEEPROMOffset is where sensor samples are written in it.
	SensorEEPROMOffset uint32 = 0x7800
)

// Config is the handshake Init sends.
type Config struct {
	// BAR is the RP1 BAR base address. Sent split across one parameter's
	// two 32-bit halves, which is why it is the only 64-bit field here.
	BAR uint64
	// I2COffset locates the I2C block inside the BAR.
	I2COffset uint32
	// Slave is the BMC's I2C address.
	Slave uint32
	// EEPROMOffset is where in the emulated EEPROM samples land.
	EEPROMOffset uint32
	// Period is how often the pTA samples. Zero leaves the TA's own default.
	Period time.Duration
}

// DefaultConfig is the wire contract as built into the firmware.
func DefaultConfig() Config {
	return Config{
		BAR:          RP1BARDefault,
		I2COffset:    RP1I2C1Offset,
		Slave:        EEPROMSlave,
		EEPROMOffset: SensorEEPROMOffset,
	}
}

// Sample is one reading.
type Sample struct {
	// TempMilliC is the SoC temperature in millidegrees Celsius. Signed:
	// the TA reports it in a 32-bit unsigned parameter, but the value is
	// two's-complement and can be below zero.
	TempMilliC int32
	// Seq increments per sample, and is what Wait blocks against.
	Seq uint32
	// Status is the TA's status word.
	Status uint32
	// I2CErrors counts push failures since boot.
	I2CErrors uint32
}

// Temperature renders the reading in degrees Celsius, three decimals.
//
// The C original composes this from a truncating divide plus abs() of the
// remainder, which drops the sign for readings between -1 and 0 °C — -500
// prints as "0.500". Splitting the sign off first keeps that case right.
func (s Sample) Temperature() string {
	t := int64(s.TempMilliC) // int64: negating math.MinInt32 overflows int32
	sign := ""
	if t < 0 {
		sign, t = "-", -t
	}
	return fmt.Sprintf("%s%d.%03d", sign, t/1000, t%1000)
}

// String is the line the C daemon prints, so existing log scrapers keep
// working against this implementation.
func (s Sample) String() string {
	return fmt.Sprintf("seq=%d soc=%s C status=0x%x i2c_errs=%d",
		s.Seq, s.Temperature(), s.Status, s.I2CErrors)
}

// Client is an open session to the sensor pTA.
type Client struct {
	ctx  *optee.Context
	sess *optee.Session
}

// Open finds the OP-TEE device and opens a session to the pTA.
func Open() (*Client, error) {
	ctx, err := optee.Open()
	if err != nil {
		return nil, err
	}
	sess, err := ctx.OpenSession(optee.MustParseUUID(UUIDString), optee.LoginPublic)
	if err != nil {
		ctx.Close()
		return nil, err
	}
	return &Client{ctx: ctx, sess: sess}, nil
}

// Close ends the session and releases the device.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	err := c.sess.Close()
	if cerr := c.ctx.Close(); err == nil {
		err = cerr
	}
	return err
}

// Init re-runs the RP1-BAR handshake.
func (c *Client) Init(cfg Config) error {
	params := initParams(cfg)
	return c.sess.Invoke(cmdInit, 0, &params)
}

// Get returns the latest cached sample without blocking.
func (c *Client) Get() (Sample, error) {
	params := getParams()
	if err := c.sess.Invoke(cmdGet, 0, &params); err != nil {
		return Sample{}, err
	}
	return sampleFromGet(params), nil
}

// The parameter marshalling is split out from the invokes so the slot
// assignments can be tested without a TEE. They are the part of this port
// most easily got wrong and the part a wrong answer is quietest about: a
// misplaced slot yields a plausible-looking sample built from the wrong
// words.

func initParams(cfg Config) optee.Params {
	return optee.Params{
		// The BAR is 64-bit and a value parameter is two 32-bit words, so
		// it is split low half then high half across the one slot.
		{
			Type: optee.ParamValueInput,
			A:    uint32(cfg.BAR), //nolint:gosec // deliberate low half of the split
			B:    uint32(cfg.BAR >> 32),
		},
		{Type: optee.ParamValueInput, A: cfg.I2COffset, B: cfg.Slave},
		{Type: optee.ParamValueInput, A: cfg.EEPROMOffset, B: millis(cfg.Period)},
		{Type: optee.ParamNone},
	}
}

func getParams() optee.Params {
	return optee.Params{
		{Type: optee.ParamValueOutput},
		{Type: optee.ParamValueOutput},
		{Type: optee.ParamValueOutput},
		{Type: optee.ParamNone},
	}
}

// sampleFromGet reads the sample GET leaves in parameters 0 and 1.
func sampleFromGet(p optee.Params) Sample {
	return Sample{
		TempMilliC: int32(p[0].A), //nolint:gosec // the TA reports two's-complement millidegrees in a u32
		Seq:        p[0].B,
		Status:     p[1].A,
		I2CErrors:  p[1].B,
	}
}

func waitParams(lastSeq uint32, timeout time.Duration) optee.Params {
	return optee.Params{
		{Type: optee.ParamValueInput, A: lastSeq, B: millis(timeout)},
		{Type: optee.ParamValueOutput},
		{Type: optee.ParamValueOutput},
		{Type: optee.ParamNone},
	}
}

// sampleFromWait reads the sample WAIT leaves one slot further along than GET
// does, because parameter 0 carried the request.
func sampleFromWait(p optee.Params) Sample {
	return Sample{
		TempMilliC: int32(p[1].A), //nolint:gosec // the TA reports two's-complement millidegrees in a u32
		Seq:        p[1].B,
		Status:     p[2].A,
		I2CErrors:  p[2].B,
	}
}

// Wait blocks until a sample newer than lastSeq is published.
//
// timeout of zero waits indefinitely, in which case Cancel from another
// goroutine is the way out. Pass the Seq of the sample previously seen; zero
// asks for the next one after the TA's current.
func (c *Client) Wait(lastSeq uint32, timeout time.Duration) (Sample, error) {
	params := waitParams(lastSeq, timeout)
	if err := c.sess.Invoke(cmdWait, cancelID, &params); err != nil {
		return Sample{}, err
	}
	return sampleFromWait(params), nil
}

// Cancel asks the TEE to abandon a blocked Wait. Safe from another goroutine.
func (c *Client) Cancel() error {
	return c.sess.Cancel(cancelID)
}

// millis converts a duration to the whole milliseconds the TA's parameters
// carry. Sub-millisecond and negative values become zero, which the ABI reads
// as "no period" for Init and "wait forever" for Wait.
func millis(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	ms := d.Milliseconds()
	if ms > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(ms) //nolint:gosec // clamped to the uint32 range just above
}
