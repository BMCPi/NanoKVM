// Package bmcsensor reads the host SoC sensor record the managed Raspberry
// Pi pushes into this BMC's emulated I2C EEPROM.
//
// The path the data takes is worth stating, because it explains why the BMC
// reads a file rather than asking anyone: an OP-TEE pseudo-TA on the Pi
// samples the BCM2712 die temperature on a secure timer and writes a
// 32-byte record over RP1 I2C1 to slave 0x50 at EEPROM offset 0x7800. The
// BMC is the slave. Linux's i2c-slave-eeprom backend answers those writes
// and exposes the resulting memory as one binary sysfs file, so by the time
// this package runs the sample is already sitting in RAM on this side of the
// wire.
//
// That makes it the one host measurement the BMC can report without the host
// cooperating over Redfish at all — it arrives from the secure world, on a
// bus the host OS does not mediate, whether or not an OS is even running.
//
// The record layout is the pTA's wire contract (optee-os
// files/plat-rpi5/pta_bmc_sensor.h) and is mirrored here, not chosen.
package bmcsensor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// The wire contract, from pta_bmc_sensor.h.
const (
	// RecordMagic is "SNSR" read as a little-endian u32.
	RecordMagic uint32 = 0x52534E53
	// RecordVersion is the only layout this package understands.
	RecordVersion uint16 = 1
	// RecordSize is sizeof(struct bmc_sensor_record). The pTA asserts at
	// build time that this fits one 64-byte EEPROM page, so a record is
	// always written atomically from the bus's point of view.
	RecordSize = 32
	// RecordOffset is where in the EEPROM the record lives: the spare
	// region of the pi-bmc EEPROM map (0x0000 vars / 0x4000 env /
	// 0x6000 SMBIOS / 0x6800 blkinfo / 0x7800 spare).
	RecordOffset = 0x7800
	// crcCovered is how much of the record the trailing CRC is taken over:
	// everything up to but excluding the CRC field itself.
	crcCovered = 28
)

// Status flags (PTA_BMC_SENSOR_STATUS_*).
const (
	// StatusTempValid means the AVS read behind this sample succeeded. When
	// it is clear the temperature field holds the last good value, not a
	// fresh one.
	StatusTempValid uint32 = 1 << 0
	// StatusI2CReady means the pTA has completed its RP1-BAR handshake.
	StatusI2CReady uint32 = 1 << 1
	// StatusLastPushOK means the pTA's previous I2C write succeeded. It
	// describes the push before this one, so a record can arrive with it
	// clear.
	StatusLastPushOK uint32 = 1 << 2
)

// Errors a malformed or absent record produces. They are distinguished
// because they mean different things operationally: ErrNoRecord is the normal
// state of a BMC whose host has never pushed, while a bad CRC means something
// wrote to this offset that should not have.
var (
	// ErrNoRecord is a region that has never been written — all zeroes, no
	// magic. Not a fault.
	ErrNoRecord = errors.New("bmcsensor: no record in the EEPROM")
	// ErrBadMagic is a non-zero region whose magic is wrong.
	ErrBadMagic = errors.New("bmcsensor: record magic does not match")
	// ErrBadCRC is a record whose contents do not match its own checksum,
	// which is what a torn or corrupted write looks like.
	ErrBadCRC = errors.New("bmcsensor: record CRC does not match")
	// ErrUnsupportedVersion is a record from a newer pTA.
	ErrUnsupportedVersion = errors.New("bmcsensor: unsupported record version")
)

// Record is one sample as the pTA wrote it.
type Record struct {
	Version uint16
	// Length is the record length the writer declared. Kept because a
	// future pTA may grow the record, and a longer one whose prefix still
	// validates is readable.
	Length uint16
	// Seq increments every sample. It is the only thing that says a record
	// is new; the temperature can legitimately repeat.
	Seq uint32
	// SoCTempMilliC is the die temperature in millidegrees Celsius, signed.
	SoCTempMilliC int32
	// UptimeSeconds is seconds since OP-TEE boot at sample time. It is the
	// host's clock, not the BMC's, so it can only be compared to itself.
	UptimeSeconds uint32
	// Status is the StatusXxx flags.
	Status uint32
}

// TempValid reports whether the temperature in this record is a fresh read
// rather than the last good value carried forward.
func (r Record) TempValid() bool { return r.Status&StatusTempValid != 0 }

// I2CReady reports whether the pTA had completed its handshake.
func (r Record) I2CReady() bool { return r.Status&StatusI2CReady != 0 }

// LastPushOK reports whether the pTA's previous push succeeded.
func (r Record) LastPushOK() bool { return r.Status&StatusLastPushOK != 0 }

// Celsius renders the temperature in degrees.
func (r Record) Celsius() float64 { return float64(r.SoCTempMilliC) / 1000 }

func (r Record) String() string {
	return fmt.Sprintf("seq=%d soc=%.3f C status=0x%x uptime=%ds",
		r.Seq, r.Celsius(), r.Status, r.UptimeSeconds)
}

// ParseRecord decodes and validates one record.
//
// Validation is not optional here. The BMC does not control what lands at
// this offset — anything on the bus can write the emulated EEPROM, and the
// backing memory starts as zeroes — so a record is only believed once its
// magic, version and CRC all agree.
func ParseRecord(b []byte) (Record, error) {
	if len(b) < RecordSize {
		return Record{}, fmt.Errorf("bmcsensor: record is %d bytes, want at least %d",
			len(b), RecordSize)
	}
	b = b[:RecordSize]

	magic := binary.LittleEndian.Uint32(b[0:4])
	if magic != RecordMagic {
		// An untouched EEPROM region reads as zeroes. Calling that
		// "corrupt" would make a BMC whose host has never pushed look
		// broken, so it gets its own error.
		if allZero(b) {
			return Record{}, ErrNoRecord
		}
		return Record{}, fmt.Errorf("%w: got 0x%08x, want 0x%08x", ErrBadMagic, magic, RecordMagic)
	}

	rec := Record{
		Version:       binary.LittleEndian.Uint16(b[4:6]),
		Length:        binary.LittleEndian.Uint16(b[6:8]),
		Seq:           binary.LittleEndian.Uint32(b[8:12]),
		SoCTempMilliC: int32(binary.LittleEndian.Uint32(b[12:16])), //nolint:gosec // two's-complement millidegrees
		UptimeSeconds: binary.LittleEndian.Uint32(b[16:20]),
		Status:        binary.LittleEndian.Uint32(b[20:24]),
	}
	// b[24:28] is the reserved word; it is not surfaced.

	// CRC before version: a mismatched CRC means the bytes cannot be
	// trusted to say what version they are.
	want := binary.LittleEndian.Uint32(b[28:32])
	if got := crc32.ChecksumIEEE(b[:crcCovered]); got != want {
		return Record{}, fmt.Errorf("%w: computed 0x%08x, record says 0x%08x", ErrBadCRC, got, want)
	}
	if rec.Version != RecordVersion {
		return Record{}, fmt.Errorf("%w: got %d, understand %d", ErrUnsupportedVersion, rec.Version, RecordVersion)
	}
	return rec, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
