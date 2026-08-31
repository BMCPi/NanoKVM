package bmcsensor

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// buildRecord assembles a current (version 3) record the way the pTA does:
// little-endian fields, a fan block, then an IEEE CRC32 over everything before
// the CRC word. Tests that want a malformed record start here and break one
// thing. The fan block is fixed but non-trivial (level 2 of 4, 49% duty) so a
// decode error in it is visible. The throttle word is left zero and its valid
// flag clear — the state of a host that has not yet handed over the mailbox;
// setThrottle populates it for the tests that exercise power health.
func buildRecord(seq uint32, tempMilliC int32, uptime, status uint32) []byte {
	b := make([]byte, RecordSize)
	binary.LittleEndian.PutUint32(b[0:4], RecordMagic)
	binary.LittleEndian.PutUint16(b[4:6], RecordVersion)
	binary.LittleEndian.PutUint16(b[6:8], RecordSize)
	binary.LittleEndian.PutUint32(b[8:12], seq)
	binary.LittleEndian.PutUint32(b[12:16], uint32(tempMilliC))
	binary.LittleEndian.PutUint32(b[16:20], uptime)
	binary.LittleEndian.PutUint32(b[20:24], status)
	// Fan block (version 2).
	b[24] = 2  // fan_level
	b[25] = 4  // fan_max_level
	b[26] = 49 // fan_duty_pct
	b[27] = FanValidFlag
	binary.LittleEndian.PutUint16(b[28:30], 0) // fan_rpm
	// b[30:32] reserved0, b[32:36] throttle, b[36:44] reserved — left zero.
	binary.LittleEndian.PutUint32(b[RecordSize-4:RecordSize], crc32.ChecksumIEEE(b[:RecordSize-4]))
	return b
}

// setThrottle stamps the version-3 throttle word into a record, sets the
// StatusThrottleValid flag in the status field, and repairs the CRC. It mutates
// b in place and returns it so tests can build a throttled record in one line.
func setThrottle(b []byte, throttle uint32) []byte {
	status := binary.LittleEndian.Uint32(b[20:24]) | StatusThrottleValid
	binary.LittleEndian.PutUint32(b[20:24], status)
	binary.LittleEndian.PutUint32(b[32:36], throttle)
	binary.LittleEndian.PutUint32(b[RecordSize-4:RecordSize], crc32.ChecksumIEEE(b[:RecordSize-4]))
	return b
}

// buildRecordV2 assembles the version-2 layout (fan block, no throttle word):
// a v2 record is byte-identical to a v3 one except for the version field and
// the meaning of bytes 32..36. Used to prove a v3 reader does not read a v2
// record's reserved zero as a "no throttling" power-health reading.
func buildRecordV2(seq uint32, tempMilliC int32, uptime, status uint32) []byte {
	b := buildRecord(seq, tempMilliC, uptime, status)
	binary.LittleEndian.PutUint16(b[4:6], 2)
	binary.LittleEndian.PutUint32(b[RecordSize-4:RecordSize], crc32.ChecksumIEEE(b[:RecordSize-4]))
	return b
}

// buildRecordV1 assembles the original 32-byte layout: no fan block, one
// reserved word, CRC over the first 28 bytes. Used to prove a current reader
// still parses a record from an older pTA.
func buildRecordV1(seq uint32, tempMilliC int32, uptime, status uint32) []byte {
	b := make([]byte, RecordSizeV1)
	binary.LittleEndian.PutUint32(b[0:4], RecordMagic)
	binary.LittleEndian.PutUint16(b[4:6], 1)
	binary.LittleEndian.PutUint16(b[6:8], RecordSizeV1)
	binary.LittleEndian.PutUint32(b[8:12], seq)
	binary.LittleEndian.PutUint32(b[12:16], uint32(tempMilliC))
	binary.LittleEndian.PutUint32(b[16:20], uptime)
	binary.LittleEndian.PutUint32(b[20:24], status)
	// b[24:28] reserved, left zero.
	binary.LittleEndian.PutUint32(b[28:32], crc32.ChecksumIEEE(b[:28]))
	return b
}

// The magic is spelled as a u32 in the header but reaches the wire as bytes.
// If the endianness were wrong the BMC would reject every record the pTA
// ever writes, so the byte order is asserted against the ASCII it encodes.
func TestRecordMagicIsSNSROnTheWire(t *testing.T) {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, RecordMagic)
	if got := string(b); got != "SNSR" {
		t.Errorf("magic bytes = %q, want %q", got, "SNSR")
	}
}

func TestParseRecordRoundTrip(t *testing.T) {
	raw := buildRecord(42, 47250, 3600, StatusTempValid|StatusI2CReady|StatusLastPushOK)
	rec, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if rec.Seq != 42 || rec.SoCTempMilliC != 47250 || rec.UptimeSeconds != 3600 {
		t.Errorf("record = %+v", rec)
	}
	if rec.Version != RecordVersion || rec.Length != RecordSize {
		t.Errorf("version/length = %d/%d", rec.Version, rec.Length)
	}
	if !rec.TempValid() || !rec.I2CReady() || !rec.LastPushOK() {
		t.Errorf("status flags not decoded: 0x%x", rec.Status)
	}
	if got := rec.Celsius(); got != 47.25 {
		t.Errorf("Celsius() = %v, want 47.25", got)
	}
}

// The fan block is the point of version 2: the fan level and duty the host now
// reports over I2C rather than by PATCHing Thermal.
func TestParseRecordDecodesFanBlock(t *testing.T) {
	raw := buildRecord(1, 45000, 10, StatusTempValid)
	rec, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if !rec.FanValid() {
		t.Fatalf("FanValid() = false, want true (flags 0x%x)", rec.FanFlags)
	}
	if rec.FanLevel != 2 || rec.FanMaxLevel != 4 || rec.FanDutyPct != 49 {
		t.Errorf("fan = level %d/%d duty %d%%, want 2/4 49%%",
			rec.FanLevel, rec.FanMaxLevel, rec.FanDutyPct)
	}
	if rec.FanRPM != 0 {
		t.Errorf("FanRPM = %d, want 0 (no tach)", rec.FanRPM)
	}
}

// The throttle word is the point of version 3: the PMIC/thermal power-health
// the host now reports over I2C. A record whose valid flag is set must decode
// both the currently-active and the latched-since-boot conditions.
func TestParseRecordDecodesThrottleBlock(t *testing.T) {
	// Under-voltage active now, and throttling has occurred at some point.
	raw := setThrottle(buildRecord(1, 45000, 10, StatusTempValid),
		ThrottleUnderVoltage|ThrottleThrottledEv)
	rec, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if !rec.ThrottleValid() {
		t.Fatalf("ThrottleValid() = false, want true (status 0x%x)", rec.Status)
	}
	if !rec.UnderVoltage() {
		t.Error("UnderVoltage() = false, want true")
	}
	if rec.Throttled() {
		t.Error("Throttled() = true, want false (only the latched bit was set)")
	}
	if !rec.ThrottledEver() {
		t.Error("ThrottledEver() = false, want true")
	}
	if rec.PowerHealthy() {
		t.Error("PowerHealthy() = true with under-voltage active, want false")
	}
}

// A clean throttle reading — valid flag set, no condition bits — is the healthy
// steady state and must read as such, distinct from "no reading".
func TestParseRecordThrottleHealthy(t *testing.T) {
	rec, err := ParseRecord(setThrottle(buildRecord(2, 45000, 10, StatusTempValid), 0))
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if !rec.ThrottleValid() || !rec.PowerHealthy() {
		t.Errorf("valid=%v healthy=%v, want true/true", rec.ThrottleValid(), rec.PowerHealthy())
	}
}

// The throttle bits are only a reading when the valid flag says the host had
// the mailbox to take them. Bits present without the flag (a pre-handoff
// record) must not be trusted.
func TestParseRecordThrottleNeedsValidFlag(t *testing.T) {
	raw := buildRecord(3, 45000, 10, StatusTempValid)
	binary.LittleEndian.PutUint32(raw[32:36], ThrottleUnderVoltage) // bits, but no flag
	binary.LittleEndian.PutUint32(raw[RecordSize-4:RecordSize], crc32.ChecksumIEEE(raw[:RecordSize-4]))
	rec, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if rec.ThrottleValid() {
		t.Error("ThrottleValid() = true without the status flag, want false")
	}
}

// A version-2 record read by this version-3 reader must not have its reserved
// bytes 32..36 read as a throttle word, even if they are non-zero: the block is
// gated on the version, not just the length.
func TestParseRecordVersion2HasNoThrottle(t *testing.T) {
	raw := buildRecordV2(4, 45000, 10, StatusTempValid)
	binary.LittleEndian.PutUint32(raw[32:36], 0xffffffff) // dirty the v2 reserved word
	binary.LittleEndian.PutUint32(raw[RecordSize-4:RecordSize], crc32.ChecksumIEEE(raw[:RecordSize-4]))
	rec, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord(v2): %v", err)
	}
	if rec.Version != 2 {
		t.Fatalf("version = %d, want 2", rec.Version)
	}
	if rec.ThrottleValid() || rec.ThrottleFlags != 0 {
		t.Errorf("v2 record surfaced a throttle word: valid=%v flags=0x%x",
			rec.ThrottleValid(), rec.ThrottleFlags)
	}
	// The fan block, which v2 does carry, must still decode.
	if !rec.FanValid() {
		t.Error("a version-2 record must still report its fan block")
	}
}

// A record from an older, version-1 pTA has no fan block and must still parse:
// the head is the contract, and a BMC should keep reporting temperature across
// a firmware downgrade.
func TestParseRecordReadsVersion1(t *testing.T) {
	rec, err := ParseRecord(buildRecordV1(9, 44000, 20, StatusTempValid|StatusI2CReady))
	if err != nil {
		t.Fatalf("ParseRecord(v1): %v", err)
	}
	if rec.Version != 1 || rec.Length != RecordSizeV1 {
		t.Errorf("version/length = %d/%d, want 1/%d", rec.Version, rec.Length, RecordSizeV1)
	}
	if rec.SoCTempMilliC != 44000 {
		t.Errorf("temp = %d, want 44000", rec.SoCTempMilliC)
	}
	if rec.FanValid() {
		t.Error("a version-1 record must not report a fan block")
	}
}

// The reader always pulls a full current-sized window; a version-1 record sits
// in the first 32 bytes with stale bytes trailing it. It must still parse off
// its declared length, ignoring the tail.
func TestParseRecordReadsVersion1InCurrentWindow(t *testing.T) {
	window := make([]byte, RecordSize)
	copy(window, buildRecordV1(3, 41000, 5, StatusTempValid))
	// Dirty the trailing bytes to prove they are not consulted.
	for i := RecordSizeV1; i < RecordSize; i++ {
		window[i] = 0xa5
	}
	rec, err := ParseRecord(window)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if rec.Seq != 3 || rec.FanValid() {
		t.Errorf("record = %+v", rec)
	}
}

// The die can read below zero on a cold start, and the field is signed in the
// pTA. Decoding it as unsigned would turn -5 °C into about 4.29 billion.
func TestParseRecordHandlesNegativeTemperature(t *testing.T) {
	rec, err := ParseRecord(buildRecord(1, -5250, 10, StatusTempValid))
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if rec.SoCTempMilliC != -5250 {
		t.Errorf("temp = %d, want -5250", rec.SoCTempMilliC)
	}
	if got := rec.Celsius(); got != -5.25 {
		t.Errorf("Celsius() = %v, want -5.25", got)
	}
}

// An EEPROM the host has never written reads as zeroes. That is the ordinary
// state of a BMC that booted before its host, not a fault, and it has to be
// distinguishable from corruption.
func TestParseRecordReportsUnwrittenRegionSeparately(t *testing.T) {
	if _, err := ParseRecord(make([]byte, RecordSize)); !errors.Is(err, ErrNoRecord) {
		t.Errorf("zeroed region = %v, want ErrNoRecord", err)
	}
}

func TestParseRecordRejectsCorruption(t *testing.T) {
	t.Run("bad magic", func(t *testing.T) {
		raw := buildRecord(1, 40000, 1, StatusTempValid)
		raw[0] ^= 0xff
		if _, err := ParseRecord(raw); !errors.Is(err, ErrBadMagic) {
			t.Errorf("err = %v, want ErrBadMagic", err)
		}
	})
	t.Run("bad crc", func(t *testing.T) {
		raw := buildRecord(1, 40000, 1, StatusTempValid)
		raw[12] ^= 0xff // corrupt the temperature, leave the CRC alone
		if _, err := ParseRecord(raw); !errors.Is(err, ErrBadCRC) {
			t.Errorf("err = %v, want ErrBadCRC", err)
		}
	})
	t.Run("unsupported version", func(t *testing.T) {
		// Version 0 is a record this reader predates; it is rejected, unlike a
		// newer version whose known prefix still validates.
		raw := buildRecord(1, 40000, 1, StatusTempValid)
		binary.LittleEndian.PutUint16(raw[4:6], 0)
		binary.LittleEndian.PutUint32(raw[RecordSize-4:RecordSize], crc32.ChecksumIEEE(raw[:RecordSize-4]))
		if _, err := ParseRecord(raw); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("err = %v, want ErrUnsupportedVersion", err)
		}
	})
	t.Run("declared length out of range", func(t *testing.T) {
		raw := buildRecord(1, 40000, 1, StatusTempValid)
		binary.LittleEndian.PutUint16(raw[6:8], 8) // shorter than the head
		if _, err := ParseRecord(raw); err == nil {
			t.Error("a record declaring an impossible length should not parse")
		}
	})
	t.Run("short buffer", func(t *testing.T) {
		if _, err := ParseRecord(buildRecord(1, 1, 1, 0)[:20]); err == nil {
			t.Error("a truncated record should not parse")
		}
	})
}

// A record longer than we know about must still read, because the pTA is
// allowed to grow one: the prefix is the contract and the CRC is located off
// the declared length.
func TestParseRecordIgnoresTrailingBytes(t *testing.T) {
	raw := append(buildRecord(7, 41000, 5, StatusTempValid), 0xde, 0xad, 0xbe, 0xef)
	rec, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if rec.Seq != 7 {
		t.Errorf("seq = %d, want 7", rec.Seq)
	}
}

// The CRC must be the same IEEE variant the pTA computes (init 0xffffffff,
// reflected polynomial 0xedb88320, final inversion). Go's crc32.IEEE is that
// variant; this pins the assumption with the pTA's own algorithm rather than
// trusting the name.
func TestCRCMatchesThePTAAlgorithm(t *testing.T) {
	ptaCRC := func(b []byte) uint32 {
		crc := uint32(0xffffffff)
		for _, c := range b {
			crc ^= uint32(c)
			for i := 0; i < 8; i++ {
				mask := -(crc & 1)
				crc = (crc >> 1) ^ (0xedb88320 & mask)
			}
		}
		return ^crc
	}
	raw := buildRecord(99, 43210, 77, StatusTempValid|StatusI2CReady)
	if got, want := crc32.ChecksumIEEE(raw[:RecordSize-4]), ptaCRC(raw[:RecordSize-4]); got != want {
		t.Fatalf("crc32.IEEE = 0x%08x, pTA algorithm = 0x%08x", got, want)
	}
}

// Only the live half of the throttle word becomes a condition. The latched
// flags never clear, so a consumer listing conditions would keep reporting a
// brownout that happened once, hours ago.
func TestLiveConditionsIgnoreTheLatchedFlags(t *testing.T) {
	latched := parse(t, setThrottle(buildRecord(1, 46000, 60, StatusTempValid),
		ThrottleUnderVoltageEv|ThrottleThrottledEv))
	if got := latched.LiveConditions(); len(got) != 0 {
		t.Errorf("LiveConditions = %v for latched-only flags, want none", got)
	}

	live := parse(t, setThrottle(buildRecord(1, 46000, 60, StatusTempValid),
		ThrottleUnderVoltage|ThrottleUnderVoltageEv))
	got := live.LiveConditions()
	if len(got) != 1 || got[0] != ConditionUnderVoltage {
		t.Errorf("LiveConditions = %v, want [UnderVoltage]", got)
	}
}

// A firmware too old to send the word must not read as an all-clear.
func TestLiveConditionsAreNilWhenTheWordIsAbsent(t *testing.T) {
	old := parse(t, buildRecordV2(1, 46000, 60, StatusTempValid))
	if old.ThrottleValid() {
		t.Fatal("a v2 record reports the throttle word as valid")
	}
	if got := old.LiveConditions(); got != nil {
		t.Errorf("LiveConditions = %v with no word, want nil", got)
	}
}

// A zero tachometer is "this host has no tach", not "the fan stopped".
func TestFanRPMValidRejectsAnAbsentTachometer(t *testing.T) {
	noTach := parse(t, buildRecord(1, 46000, 60, StatusTempValid))
	if !noTach.FanValid() {
		t.Fatal("the fixture's fan block should be valid")
	}
	if noTach.FanRPMValid() {
		t.Error("a zero RPM was reported as a speed; it means no tach capture")
	}
}

func parse(t *testing.T, b []byte) Record {
	t.Helper()
	r, err := ParseRecord(b)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	return r
}
