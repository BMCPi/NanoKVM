package bmcsensor

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// buildRecord assembles a record the way the pTA does: little-endian fields,
// then an IEEE CRC32 over everything before the CRC word. Tests that want a
// malformed record start here and break one thing.
func buildRecord(seq uint32, tempMilliC int32, uptime, status uint32) []byte {
	b := make([]byte, RecordSize)
	binary.LittleEndian.PutUint32(b[0:4], RecordMagic)
	binary.LittleEndian.PutUint16(b[4:6], RecordVersion)
	binary.LittleEndian.PutUint16(b[6:8], RecordSize)
	binary.LittleEndian.PutUint32(b[8:12], seq)
	binary.LittleEndian.PutUint32(b[12:16], uint32(tempMilliC))
	binary.LittleEndian.PutUint32(b[16:20], uptime)
	binary.LittleEndian.PutUint32(b[20:24], status)
	// b[24:28] reserved, left zero.
	binary.LittleEndian.PutUint32(b[28:32], crc32.ChecksumIEEE(b[:crcCovered]))
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
		raw := buildRecord(1, 40000, 1, StatusTempValid)
		binary.LittleEndian.PutUint16(raw[4:6], 2)
		binary.LittleEndian.PutUint32(raw[28:32], crc32.ChecksumIEEE(raw[:crcCovered]))
		if _, err := ParseRecord(raw); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("err = %v, want ErrUnsupportedVersion", err)
		}
	})
	t.Run("short buffer", func(t *testing.T) {
		if _, err := ParseRecord(buildRecord(1, 1, 1, 0)[:20]); err == nil {
			t.Error("a truncated record should not parse")
		}
	})
}

// A record longer than we know about must still read, because the pTA is
// allowed to grow one: the prefix is the contract.
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
	if got, want := crc32.ChecksumIEEE(raw[:crcCovered]), ptaCRC(raw[:crcCovered]); got != want {
		t.Fatalf("crc32.IEEE = 0x%08x, pTA algorithm = 0x%08x", got, want)
	}
}
