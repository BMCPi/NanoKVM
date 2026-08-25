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
	// RecordVersion is the current record layout this package writes and
	// expects. Older versions whose prefix still validates are read too
	// (see ParseRecord): the trailing CRC is located from the record's own
	// declared length, and each block is gated on the record's version, so
	// any older writer and this reader interoperate.
	RecordVersion uint16 = 3
	// RecordSizeV1 is the original layout — the common head plus one
	// reserved word, temperature only.
	RecordSizeV1 = 32
	// RecordSize is sizeof(struct bmc_sensor_record) at the current
	// version: the v1 head plus the fan block and reserved words. The pTA
	// asserts at build time that this fits one 64-byte EEPROM page, so a
	// record is always written atomically from the bus's point of view.
	RecordSize = 48
	// RecordMaxSize bounds a record's declared length: one EEPROM page.
	RecordMaxSize = 64
	// RecordOffset is where in the EEPROM the record lives: the spare
	// region of the pi-bmc EEPROM map (0x0000 vars / 0x4000 env /
	// 0x6000 SMBIOS / 0x6800 blkinfo / 0x7800 spare).
	RecordOffset = 0x7800
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
	// StatusThrottleValid means the pTA read the firmware GET_THROTTLED word
	// for this sample (it had the VPU mailbox, i.e. the host is past
	// ExitBootServices). When clear, the throttle flags are not a reading.
	StatusThrottleValid uint32 = 1 << 4
)

// Throttle flags (BMC_SENSOR_THROTTLE_*), valid from record version 3 when
// StatusThrottleValid is set. These are the firmware's GET_THROTTLED word: the
// low bits are conditions active right now, the high bits latch that the
// condition has occurred at least once since the host booted. Under-voltage
// and frequency capping are the PMIC's signals; the soft temperature limit is
// the SoC thermal block's.
const (
	ThrottleUnderVoltage    uint32 = 1 << 0
	ThrottleFreqCapped      uint32 = 1 << 1
	ThrottleThrottled       uint32 = 1 << 2
	ThrottleSoftTempLimit   uint32 = 1 << 3
	ThrottleUnderVoltageEv  uint32 = 1 << 16
	ThrottleFreqCappedEv    uint32 = 1 << 17
	ThrottleThrottledEv     uint32 = 1 << 18
	ThrottleSoftTempLimitEv uint32 = 1 << 19
)

// Fan flags (BMC_SENSOR_FAN_*), valid from record version 2.
const (
	// FanValidFlag means the record's fan block holds a real reading rather
	// than a placeholder.
	FanValidFlag uint8 = 1 << 0
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
	// FanLevel is the commanded cooling level (0..FanMaxLevel). Present from
	// record version 2; zero on a version-1 record.
	FanLevel uint8
	// FanMaxLevel is the highest cooling level the host exposes.
	FanMaxLevel uint8
	// FanDutyPct is the PWM duty of the commanded level, 0..100.
	FanDutyPct uint8
	// FanFlags is the FanXxxFlag bits.
	FanFlags uint8
	// FanRPM is the measured tachometer speed; 0 means not measured (the
	// host has no tach capture yet).
	FanRPM uint16
	// hasFan records whether this record actually carried a fan block, so a
	// version-1 record is distinguishable from a v2 one reporting level 0.
	hasFan bool
	// ThrottleFlags is the firmware GET_THROTTLED word (ThrottleXxx bits):
	// PMIC under-voltage/frequency-capping and the SoC soft-temperature
	// limit, current and latched-since-boot. Present from record version 3,
	// and a real reading only when ThrottleValid reports true.
	ThrottleFlags uint32
	// hasThrottle records whether this record carried the version-3 throttle
	// word at all, so an older record is distinguishable from a v3 one whose
	// power health happens to read zero.
	hasThrottle bool
}

// TempValid reports whether the temperature in this record is a fresh read
// rather than the last good value carried forward.
func (r Record) TempValid() bool { return r.Status&StatusTempValid != 0 }

// I2CReady reports whether the pTA had completed its handshake.
func (r Record) I2CReady() bool { return r.Status&StatusI2CReady != 0 }

// LastPushOK reports whether the pTA's previous push succeeded.
func (r Record) LastPushOK() bool { return r.Status&StatusLastPushOK != 0 }

// FanValid reports whether this record carried a usable fan block. It is
// false for a version-1 record, which had no fan fields at all.
func (r Record) FanValid() bool { return r.hasFan && r.FanFlags&FanValidFlag != 0 }

// throttleNowMask is the set of throttle conditions that are active right now
// (as opposed to the latched-since-boot bits).
const throttleNowMask = ThrottleUnderVoltage | ThrottleFreqCapped |
	ThrottleThrottled | ThrottleSoftTempLimit

// ThrottleValid reports whether this record carried a live GET_THROTTLED
// reading. It is false for a pre-version-3 record, and for a v3 record the host
// wrote before ExitBootServices (when it did not yet own the VPU mailbox), so
// the predicates below only mean something when it is true.
func (r Record) ThrottleValid() bool {
	return r.hasThrottle && r.Status&StatusThrottleValid != 0
}

// UnderVoltage, FrequencyCapped, Throttled and SoftTempLimited report the
// conditions active in this sample; the *Ever variants report whether the
// condition has occurred at least once since the host booted. All are
// meaningful only when ThrottleValid is true.
func (r Record) UnderVoltage() bool    { return r.ThrottleFlags&ThrottleUnderVoltage != 0 }
func (r Record) FrequencyCapped() bool { return r.ThrottleFlags&ThrottleFreqCapped != 0 }
func (r Record) Throttled() bool       { return r.ThrottleFlags&ThrottleThrottled != 0 }
func (r Record) SoftTempLimited() bool { return r.ThrottleFlags&ThrottleSoftTempLimit != 0 }

func (r Record) UnderVoltageEver() bool    { return r.ThrottleFlags&ThrottleUnderVoltageEv != 0 }
func (r Record) FrequencyCappedEver() bool { return r.ThrottleFlags&ThrottleFreqCappedEv != 0 }
func (r Record) ThrottledEver() bool       { return r.ThrottleFlags&ThrottleThrottledEv != 0 }
func (r Record) SoftTempLimitedEver() bool { return r.ThrottleFlags&ThrottleSoftTempLimitEv != 0 }

// PowerHealthy reports that no power or thermal limiting is active right now.
// It is only meaningful when ThrottleValid is true: a record with no throttle
// reading has no flags set and so reports healthy by default.
func (r Record) PowerHealthy() bool { return r.ThrottleFlags&throttleNowMask == 0 }

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
	if len(b) < RecordSizeV1 {
		return Record{}, fmt.Errorf("bmcsensor: record is %d bytes, want at least %d",
			len(b), RecordSizeV1)
	}

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

	version := binary.LittleEndian.Uint16(b[4:6])
	length := int(binary.LittleEndian.Uint16(b[6:8]))
	// The declared length locates the trailing CRC. Trusting it before the
	// CRC is checked is safe — a wrong length simply fails the CRC below —
	// but it must still name a window that fits one page and the bytes we
	// were given.
	if length < RecordSizeV1 || length > RecordMaxSize || length > len(b) {
		return Record{}, fmt.Errorf("bmcsensor: record declares length %d, out of range [%d,%d]",
			length, RecordSizeV1, RecordMaxSize)
	}

	// CRC before version or length are believed: a mismatch means the bytes
	// cannot be trusted to say what they are.
	want := binary.LittleEndian.Uint32(b[length-4 : length])
	if got := crc32.ChecksumIEEE(b[:length-4]); got != want {
		return Record{}, fmt.Errorf("%w: computed 0x%08x, record says 0x%08x", ErrBadCRC, got, want)
	}

	// Only a version this reader predates is unreadable. v1 and v2 share the
	// common head; v2+ additionally carries the fan block, which older
	// readers skip via the length-driven CRC above.
	if version < 1 {
		return Record{}, fmt.Errorf("%w: got %d, understand up to %d", ErrUnsupportedVersion, version, RecordVersion)
	}

	rec := Record{
		Version:       version,
		Length:        uint16(length),
		Seq:           binary.LittleEndian.Uint32(b[8:12]),
		SoCTempMilliC: int32(binary.LittleEndian.Uint32(b[12:16])), //nolint:gosec // two's-complement millidegrees
		UptimeSeconds: binary.LittleEndian.Uint32(b[16:20]),
		Status:        binary.LittleEndian.Uint32(b[20:24]),
	}

	// Fan block (version 2+), bytes 24..29. On a v1 record bytes 24..27 are
	// a reserved word and are not surfaced.
	if version >= 2 && length >= RecordSize {
		rec.FanLevel = b[24]
		rec.FanMaxLevel = b[25]
		rec.FanDutyPct = b[26]
		rec.FanFlags = b[27]
		rec.FanRPM = binary.LittleEndian.Uint16(b[28:30])
		rec.hasFan = true
	}

	// Throttle word (version 3+), bytes 32..36 — the slot a version-2 record
	// left reserved. Gated on version so a v2 record's reserved zero is not
	// read as a "no throttling" power-health reading.
	if version >= 3 && length >= RecordSize {
		rec.ThrottleFlags = binary.LittleEndian.Uint32(b[32:36])
		rec.hasThrottle = true
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
