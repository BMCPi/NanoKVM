package blkinfo

import (
	"encoding/binary"
	"errors"
	"testing"
)

// region builds a raw EEPROM region exactly as lib/blkinfo_i2c.c writes it:
// "BLK1" magic, u16le length, JSON, then whatever old bytes follow.
func region(payload string, size int) []byte {
	raw := make([]byte, size)
	copy(raw, "BLK1")
	binary.LittleEndian.PutUint16(raw[4:6], uint16(len(payload)))
	copy(raw[6:], payload)
	return raw
}

func TestParseRoundTrip(t *testing.T) {
	payload := `{"v":1,"drives":[` +
		`{"if":"nvme","dev":0,"vendor":"","product":"Samsung SSD 990 EVO 1TB","rev":"0B2QKXJ7","removable":0,"size":1000204886016},` +
		`{"if":"mmc","dev":0,"vendor":"Man 00001b","product":"EB1QT","rev":"1.0","removable":0,"size":31268536320},` +
		`{"if":"usb","dev":0,"vendor":"Linux","product":"File-Stor Gadget","rev":"6.18","removable":1,"size":537936896}]}`

	inv, err := Parse(region(payload, 0x1000))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if inv.Version != 1 {
		t.Errorf("Version = %d, want 1", inv.Version)
	}
	if len(inv.Drives) != 3 {
		t.Fatalf("Drives = %d, want 3", len(inv.Drives))
	}
	nvme := inv.Drives[0]
	if nvme.Interface != "nvme" || nvme.Devnum != 0 ||
		nvme.Product != "Samsung SSD 990 EVO 1TB" ||
		nvme.Revision != "0B2QKXJ7" || nvme.SizeBytes != 1000204886016 {
		t.Errorf("nvme drive mismapped: %+v", nvme)
	}
	if usb := inv.Drives[2]; usb.Removable != 1 || usb.SizeBytes != 537936896 {
		t.Errorf("usb drive mismapped: %+v", usb)
	}
}

// A blank or garbage region must report ErrNoInventory, not a JSON error —
// callers use it to distinguish "host has not booted yet".
func TestParseBlankRegion(t *testing.T) {
	if _, err := Parse(make([]byte, 0x1000)); !errors.Is(err, ErrNoInventory) {
		t.Errorf("blank region: err = %v, want ErrNoInventory", err)
	}
	if _, err := Parse([]byte{0xff, 0xff}); !errors.Is(err, ErrNoInventory) {
		t.Errorf("short region: err = %v, want ErrNoInventory", err)
	}
}

// A length running past the region must be rejected, not sliced OOB.
func TestParseBogusLength(t *testing.T) {
	raw := region(`{"v":1,"drives":[]}`, 64)
	binary.LittleEndian.PutUint16(raw[4:6], 5000)
	if _, err := Parse(raw); err == nil {
		t.Error("oversized length accepted")
	}
}

// fixtureBackend serves a fixed region at a fixed offset, mimicking the
// shared-EEPROM file backend.
type fixtureBackend struct{ data []byte }

func (f *fixtureBackend) ReadAt(off int, p []byte) error {
	copy(p, f.data[off:])
	return nil
}
func (f *fixtureBackend) Size() int { return len(f.data) }

func TestStoreLoadAtOffset(t *testing.T) {
	eeprom := make([]byte, 0x8000)
	copy(eeprom[0x6800:], region(`{"v":1,"drives":[{"if":"nvme","dev":0,"size":512}]}`, 0x1000))

	s := NewStore(&fixtureBackend{data: eeprom}, 0x6800, 0x1000)
	inv, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(inv.Drives) != 1 || inv.Drives[0].SizeBytes != 512 {
		t.Errorf("Load = %+v, want one 512-byte nvme drive", inv)
	}
}
