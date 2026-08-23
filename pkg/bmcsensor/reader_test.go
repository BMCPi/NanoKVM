package bmcsensor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeEEPROM writes a file shaped like the sysfs attribute: the full emulated
// part, with the record at its real offset. Reading the record out of a
// 64 KiB file at 0x7800 is the behaviour under test, so the fixture is not
// shortened to just the record.
func fakeEEPROM(t *testing.T, record []byte) string {
	t.Helper()
	buf := make([]byte, 64*1024)
	copy(buf[RecordOffset:], record)
	path := filepath.Join(t.TempDir(), "slave-eeprom")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReaderReadsTheRecordAtItsOffset(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(5, 46500, 120, StatusTempValid|StatusI2CReady))
	r := NewReaderAt(path, DefaultStaleAfter)
	if !r.Available() {
		t.Fatal("Available() = false for an existing attribute")
	}
	got, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Seq != 5 || got.SoCTempMilliC != 46500 {
		t.Errorf("reading = %+v", got.Record)
	}
	if got.Stale {
		t.Error("a freshly observed sequence should not be stale")
	}
}

// The host powering off leaves its last record in place, still parsing
// perfectly. Reporting that forever would show a live die temperature for a
// machine that is off, so an unchanging sequence has to go stale.
func TestReadingGoesStaleWhenTheSequenceStops(t *testing.T) {
	path := fakeEEPROM(t, buildRecord(9, 44000, 60, StatusTempValid))
	r := NewReaderAt(path, 30*time.Second)

	base := time.Unix(1_700_000_000, 0)
	now := base
	r.now = func() time.Time { return now }

	first, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first.Stale {
		t.Fatal("first observation should not be stale")
	}

	// Same record, well past the window: the host stopped pushing.
	now = base.Add(31 * time.Second)
	again, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !again.Stale {
		t.Error("an unchanged sequence past the window should be stale")
	}
	if !again.At.Equal(first.At) {
		t.Errorf("At moved without the sequence changing: %v -> %v", first.At, again.At)
	}

	// A new push clears it again.
	if err := os.WriteFile(path, eepromWith(buildRecord(10, 44500, 91, StatusTempValid)), 0o600); err != nil {
		t.Fatal(err)
	}
	now = base.Add(32 * time.Second)
	fresh, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fresh.Stale {
		t.Error("a new sequence number should clear staleness")
	}
	if !fresh.At.Equal(now) {
		t.Errorf("At = %v, want the observation time %v", fresh.At, now)
	}
}

func eepromWith(record []byte) []byte {
	buf := make([]byte, 64*1024)
	copy(buf[RecordOffset:], record)
	return buf
}

// A kernel without the slave EEPROM has no attribute at all. That is a
// different situation from a host that has not pushed, and callers decide
// whether to offer the sensor on it.
func TestReaderReportsAMissingAttribute(t *testing.T) {
	r := NewReaderAt(filepath.Join(t.TempDir(), "absent"), DefaultStaleAfter)
	if r.Available() {
		t.Error("Available() = true for a missing attribute")
	}
	if _, err := r.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read err = %v, want a not-exist error", err)
	}
}

// An EEPROM too small to hold the offset means the wrong part is emulated —
// an 8-bit 24c02 where a 16-bit one is needed. That must not read as a bad
// sample, because the fix is a device-tree change, not a retry.
func TestReaderReportsAnUndersizedEEPROM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slave-eeprom")
	if err := os.WriteFile(path, make([]byte, 256), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewReaderAt(path, DefaultStaleAfter).Read()
	if err == nil {
		t.Fatal("reading past the end of the attribute should fail")
	}
	if errors.Is(err, ErrNoRecord) || errors.Is(err, ErrBadCRC) {
		t.Errorf("err = %v, should not read as a record problem", err)
	}
}

func TestReaderSurfacesAnUnwrittenRegion(t *testing.T) {
	path := fakeEEPROM(t, nil) // all zeroes
	if _, err := NewReaderAt(path, DefaultStaleAfter).Read(); !errors.Is(err, ErrNoRecord) {
		t.Errorf("err = %v, want ErrNoRecord", err)
	}
}
