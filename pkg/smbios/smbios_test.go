package smbios

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

const (
	testRegionSize = 0x800
	testOffset     = 0x6000
	testChipSize   = 0x8000
)

// type1 builds an SMBIOS System Information structure: a 4-byte header, a
// 23-byte formatted area (length 0x1b total), then the string table.
func type1(uuid [16]byte, strs ...string) []byte {
	var b bytes.Buffer
	b.WriteByte(1)    // type
	b.WriteByte(0x1b) // length: header + formatted area
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	b.WriteByte(1) // manufacturer -> string 1
	b.WriteByte(2) // product name -> string 2
	b.WriteByte(3) // version      -> string 3
	b.WriteByte(4) // serial       -> string 4
	b.Write(uuid[:])
	b.WriteByte(6) // wake-up type: power switch
	b.WriteByte(5) // SKU          -> string 5
	b.WriteByte(6) // family       -> string 6
	for _, s := range strs {
		b.WriteString(s)
		b.WriteByte(0)
	}
	b.WriteByte(0) // end of the string table
	return b.Bytes()
}

// type0 builds a BIOS Information structure (length 0x18, through the Embedded
// Controller firmware release bytes at offsets 16h/17h). strs are vendor,
// version, release date.
func type0(ecMajor, ecMinor uint8, strs ...string) []byte {
	var b bytes.Buffer
	b.WriteByte(0)         // type
	b.WriteByte(0x18)      // length: header + formatted through EC minor
	le(&b, uint16(0x0000)) // handle
	b.WriteByte(1)         // vendor       -> string 1
	b.WriteByte(2)         // bios version -> string 2
	le(&b, uint16(0xF000)) // starting address segment
	b.WriteByte(3)         // release date -> string 3
	b.WriteByte(0)         // rom size
	le(&b, uint64(0))      // bios characteristics
	b.WriteByte(0)         // characteristics ext 1 (offset 0x12)
	b.WriteByte(0)         // characteristics ext 2 (offset 0x13)
	b.WriteByte(0)         // system bios major release (0x14)
	b.WriteByte(0)         // system bios minor release (0x15)
	b.WriteByte(ecMajor)   // EC firmware major release (0x16)
	b.WriteByte(ecMinor)   // EC firmware minor release (0x17)
	for _, s := range strs {
		b.WriteString(s)
		b.WriteByte(0)
	}
	b.WriteByte(0) // string-set terminator
	return b.Bytes()
}

// type45 builds a Firmware Inventory Information structure (length 0x18, with
// no associated component handles). strs are the component name, version,
// firmware ID, release date and manufacturer.
func type45(strs ...string) []byte {
	var b bytes.Buffer
	b.WriteByte(45)                    // type
	b.WriteByte(0x18)                  // length: formatted area, no handles
	le(&b, uint16(0x4500))             // handle
	b.WriteByte(1)                     // component name -> string 1
	b.WriteByte(2)                     // version        -> string 2
	b.WriteByte(0)                     // version format: free-form
	b.WriteByte(3)                     // firmware ID    -> string 3
	b.WriteByte(0)                     // ID format: free-form
	b.WriteByte(4)                     // release date   -> string 4
	b.WriteByte(5)                     // manufacturer   -> string 5
	b.WriteByte(0)                     // lowest supported version: none
	le(&b, uint64(0xffffffffffffffff)) // image size: unknown
	le(&b, uint16(1))                  // characteristics: updatable
	b.WriteByte(4)                     // state: enabled
	b.WriteByte(0)                     // associated component count
	for _, s := range strs {
		b.WriteString(s)
		b.WriteByte(0)
	}
	b.WriteByte(0) // string-set terminator
	return b.Bytes()
}

// le appends v to b in little-endian order (SMBIOS is little-endian).
func le(b *bytes.Buffer, v any) {
	if err := binary.Write(b, binary.LittleEndian, v); err != nil {
		panic(err)
	}
}

// type16 builds a Physical Memory Array (length 0x17, no strings). errCorr is
// the raw SMBIOS Memory Error Correction byte; devices is the socket count.
func type16(errCorr uint8, devices uint16) []byte {
	var b bytes.Buffer
	b.WriteByte(16)            // type
	b.WriteByte(0x17)          // length
	le(&b, uint16(0x1000))     // handle
	b.WriteByte(3)             // location: system board
	b.WriteByte(3)             // use: system memory
	b.WriteByte(errCorr)       // memory error correction
	le(&b, uint32(0x80000000)) // max capacity -> use extended
	le(&b, uint16(0xFFFE))     // memory error information handle
	le(&b, devices)            // number of memory devices
	le(&b, uint64(0))          // extended maximum capacity
	b.WriteByte(0)             // empty string set: two null bytes
	b.WriteByte(0)
	return b.Bytes()
}

// type17 builds a Memory Device (length 0x28) with the given string set. mem
// is the raw Memory Type byte, ff the raw Form Factor byte, sizeMB the size.
func type17(mem, ff uint8, sizeMB uint16, strs ...string) []byte {
	var b bytes.Buffer
	b.WriteByte(17)        // type
	b.WriteByte(0x28)      // length
	le(&b, uint16(0x1100)) // handle
	le(&b, uint16(0x1000)) // physical memory array handle
	le(&b, uint16(0xFFFE)) // memory error information handle
	le(&b, uint16(32))     // total width
	le(&b, uint16(32))     // data width
	le(&b, sizeMB)         // size (MB, bit 15 clear)
	b.WriteByte(ff)        // form factor
	b.WriteByte(0)         // device set: none
	b.WriteByte(1)         // device locator -> string 1
	b.WriteByte(2)         // bank locator   -> string 2
	b.WriteByte(mem)       // memory type
	le(&b, uint16(0x0080)) // type detail: synchronous
	le(&b, uint16(4267))   // speed (MT/s)
	b.WriteByte(3)         // manufacturer  -> string 3
	b.WriteByte(4)         // serial number -> string 4
	b.WriteByte(0)         // asset tag     -> none
	b.WriteByte(5)         // part number   -> string 5
	b.WriteByte(0)         // attributes
	le(&b, uint32(0))      // extended size
	le(&b, uint16(4267))   // configured memory speed
	le(&b, uint16(1100))   // minimum voltage
	le(&b, uint16(1100))   // maximum voltage
	le(&b, uint16(1100))   // configured voltage
	for _, s := range strs {
		b.WriteString(s)
		b.WriteByte(0)
	}
	b.WriteByte(0) // string-set terminator
	return b.Bytes()
}

// type9 builds a System Slot (length 0x0C) whose designation is string 1.
func type9(designation string) []byte {
	var b bytes.Buffer
	b.WriteByte(9)         // type
	b.WriteByte(0x0C)      // length
	le(&b, uint16(0x0900)) // handle
	b.WriteByte(1)         // slot designation -> string 1
	b.WriteByte(0xA5)      // slot type: PCI Express
	b.WriteByte(0x08)      // data bus width
	b.WriteByte(0x03)      // current usage: available
	b.WriteByte(0x03)      // slot length
	le(&b, uint16(0))      // slot ID
	b.WriteByte(0)         // characteristics 1
	b.WriteString(designation)
	b.WriteByte(0)
	b.WriteByte(0) // string-set terminator
	return b.Bytes()
}

// endOfTable builds the type 127 terminator.
func endOfTable() []byte {
	return []byte{127, 4, 0x7f, 0x00, 0x00, 0x00}
}

// buildRegion assembles the bytes exactly as U-Boot lays them out: the _SM3_
// entry point, then the structure table at ALIGN(entry.length, 16).
func buildRegion(t *testing.T, tables []byte) []byte {
	t.Helper()
	region := make([]byte, testRegionSize)
	copy(region[align(entryPointLen, tableAlign):], tables)

	ep := entryPoint{
		Anchor:       anchor,
		Length:       entryPointLen,
		MajorVer:     3,
		MinorVer:     7,
		DocRev:       0,
		Revision:     1,
		TableMaxSize: uint32(len(tables)),
		// A DRAM address, as U-Boot leaves it. Parsing must ignore this.
		TableAddress: 0x3f737020,
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &ep); err != nil {
		t.Fatalf("encode entry point: %v", err)
	}
	b := buf.Bytes()
	var sum uint8
	for _, c := range b {
		sum += c
	}
	b[5] = 0 - sum // 8-bit sum-to-zero checksum
	copy(region, b)
	return region
}

// concat joins structure blobs into one table stream.
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func sampleRegion(t *testing.T) []byte {
	t.Helper()
	uuid := [16]byte{
		0x2e, 0xd6, 0xcc, 0x84, 0x87, 0xe3, 0x46, 0x79,
		0xb1, 0x41, 0xe0, 0x00, 0x52, 0x50, 0x69, 0x00,
	}
	tables := concat(
		type1(uuid, "Raspberry Pi", "Raspberry Pi 5 Model B Rev 1.1", "1.1",
			"79a6e38784ccd62e", "E04171", "Raspberry Pi"),
		// Single-bit ECC, one memory device; a 16 GiB LPDDR4 "Row of chips"
		// package; and a PCIe slot. The enum bytes (0x05, 0x1E, 0x0B) are the
		// exact values go-smbios's own String() would misdecode.
		type16(0x05, 1),
		type17(0x1E, 0x0B, 16384, "P0", "CH0", "Micron", "SN12345", "MT53E2G32"),
		type9("PCIe"),
		endOfTable(),
	)
	return buildRegion(t, tables)
}

func TestParseFirmwareInventory(t *testing.T) {
	// Type 0 (U-Boot's own BIOS identity) and Type 45 (the rpi-eeprom boot
	// firmware) are independent: the eeprom version must not leak into the
	// Type 0 fields, and vice versa.
	region := buildRegion(t, concat(
		type0(0xff, 0xff, "U-Boot", "U-Boot 2026.04", "05/20/2026"),
		type45("Raspberry Pi Boot Firmware", "2026.525",
			"086b83e3332dfc8927c56762771d082f3077a1ae",
			"2026-05-25", "Raspberry Pi"),
		endOfTable(),
	))
	info, err := Parse(region)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(info.FirmwareInventory) != 1 {
		t.Fatalf("FirmwareInventory has %d entries, want 1", len(info.FirmwareInventory))
	}
	fw := info.FirmwareInventory[0]
	if fw.Name != "Raspberry Pi Boot Firmware" {
		t.Errorf("Name = %q", fw.Name)
	}
	// "2026.525" -> the day is the right two digits of the minor segment.
	if fw.Version != "2026-05-25" {
		t.Errorf("Version = %q, want %q", fw.Version, "2026-05-25")
	}
	if fw.RawVersion != "2026.525" {
		t.Errorf("RawVersion = %q, want %q", fw.RawVersion, "2026.525")
	}
	if fw.ID != "086b83e3332dfc8927c56762771d082f3077a1ae" {
		t.Errorf("ID = %q", fw.ID)
	}
	if fw.ReleaseDate != "2026-05-25" {
		t.Errorf("ReleaseDate = %q", fw.ReleaseDate)
	}
	if fw.Manufacturer != "Raspberry Pi" {
		t.Errorf("Manufacturer = %q", fw.Manufacturer)
	}
	if info.BootFirmwareVersion != "2026-05-25" {
		t.Errorf("BootFirmwareVersion = %q", info.BootFirmwareVersion)
	}
	// Type 0 still describes U-Boot, untouched by the firmware inventory.
	if info.BIOSVersion != "U-Boot 2026.04" || info.BIOSDate != "05/20/2026" {
		t.Errorf("Type 0 = %q/%q, want U-Boot's own identity",
			info.BIOSVersion, info.BIOSDate)
	}
}

func TestFirmwareVersionRendering(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		// Date-versioned: the day is the right two digits of the minor.
		{"2026.525", "2026-05-25"},
		{"2026.1202", "2026-12-02"},
		// Not date-versioned: reported unchanged.
		{"1.2.3", "1.2.3"},
		{"086b83e3", "086b83e3"},
		// Impossible date: left as-is rather than silently "corrected".
		{"2026.230", "2026.230"},
	} {
		// Every string must be non-empty: an empty string in an SMBIOS
		// string set reads as the set terminator and truncates the table.
		region := buildRegion(t, concat(
			type45("Boot Firmware", tc.raw, "id", "2026-01-01", "Raspberry Pi"),
			endOfTable(),
		))
		info, err := Parse(region)
		if err != nil {
			t.Fatalf("Parse(%s): %v", tc.raw, err)
		}
		if len(info.FirmwareInventory) != 1 {
			t.Fatalf("%s: got %d entries", tc.raw, len(info.FirmwareInventory))
		}
		if got := info.FirmwareInventory[0].Version; got != tc.want {
			t.Errorf("raw %q: Version = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestParseSystemInformation(t *testing.T) {
	info, err := Parse(sampleRegion(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, tc := range []struct{ name, got, want string }{
		{"Manufacturer", info.Manufacturer, "Raspberry Pi"},
		{"Product", info.Product, "Raspberry Pi 5 Model B Rev 1.1"},
		{"Version", info.Version, "1.1"},
		{"Serial", info.Serial, "79a6e38784ccd62e"},
		{"SKU", info.SKU, "E04171"},
		{"Family", info.Family, "Raspberry Pi"},
		{"SMBIOSVersion", info.SMBIOSVersion, "3.7.0"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// The UUID is the whole point of reading SMBIOS: it exists nowhere in
	// the U-Boot environment.
	if info.UUID == "" {
		t.Error("UUID is empty")
	}
}

// The type 16/17/9 tables U-Boot now exports must map onto the memory summary
// and slot inventory, decoding the enum bytes correctly along the way.
func TestParseMemoryAndSlots(t *testing.T) {
	info, err := Parse(sampleRegion(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if info.MemoryTotalMB != 16384 {
		t.Errorf("MemoryTotalMB = %d, want 16384", info.MemoryTotalMB)
	}
	if info.MemorySlots != 1 {
		t.Errorf("MemorySlots = %d, want 1", info.MemorySlots)
	}
	if info.MemoryErrorCorrection != "Single-bit ECC" {
		t.Errorf("MemoryErrorCorrection = %q, want Single-bit ECC", info.MemoryErrorCorrection)
	}
	if len(info.Slots) != 1 || info.Slots[0] != "PCIe" {
		t.Errorf("Slots = %v, want [PCIe]", info.Slots)
	}

	if len(info.Memory) != 1 {
		t.Fatalf("Memory has %d modules, want 1", len(info.Memory))
	}
	m := info.Memory[0]
	for _, tc := range []struct{ name, got, want string }{
		{"Type", m.Type, "LPDDR4"},
		{"FormFactor", m.FormFactor, "Row of chips"},
		{"Manufacturer", m.Manufacturer, "Micron"},
		{"PartNumber", m.PartNumber, "MT53E2G32"},
		{"SerialNumber", m.SerialNumber, "SN12345"},
		{"Locator", m.Locator, "P0"},
		{"BankLocator", m.BankLocator, "CH0"},
	} {
		if tc.got != tc.want {
			t.Errorf("Memory[0].%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"SizeMB", m.SizeMB, 16384},
		{"SpeedMTs", m.SpeedMTs, 4267},
		{"ConfiguredSpeedMTs", m.ConfiguredSpeedMTs, 4267},
		{"DataWidthBits", m.DataWidthBits, 32},
		{"TotalWidthBits", m.TotalWidthBits, 32},
	} {
		if tc.got != tc.want {
			t.Errorf("Memory[0].%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// go-smbios v0.3.4 mis-decodes these enums (it indexes from 0 while SMBIOS
// numbers from 0x01), so our decoders own the mapping. Pin the values that
// would otherwise shift, and confirm the placeholders resolve to "".
func TestMemoryEnumDecodersMatchSpec(t *testing.T) {
	for _, tc := range []struct {
		raw  int
		want string
	}{
		{0x1E, "LPDDR4"}, // go-smbios would say "HBM2"
		{0x1A, "DDR4"},   // go-smbios would say "LPDDR3"
		{0x22, "DDR5"},
		{0x23, "LPDDR5"},
		{0x01, ""}, // Other
		{0x02, ""}, // Unknown
		{0x15, ""}, // Reserved
		{0x17, ""}, // Reserved
	} {
		if got := memoryTypeName(tc.raw); got != tc.want {
			t.Errorf("memoryTypeName(%#x) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	if got := formFactorName(0x0B); got != "Row of chips" { // go-smbios: "RIMM"
		t.Errorf("formFactorName(0x0B) = %q, want Row of chips", got)
	}
	if got := formFactorName(0x0D); got != "SODIMM" { // go-smbios: "SRIMM"
		t.Errorf("formFactorName(0x0D) = %q, want SODIMM", got)
	}

	for _, tc := range []struct {
		raw  int
		want string
	}{
		{0x02, ""},               // Unknown — go-smbios: "None"
		{0x03, "None"},           // go-smbios: "Parity"
		{0x05, "Single-bit ECC"}, // go-smbios: "Multi-bit ECC"
		{0x06, "Multi-bit ECC"},
	} {
		if got := memoryErrorCorrectionName(tc.raw); got != tc.want {
			t.Errorf("memoryErrorCorrectionName(%#x) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// A blank EEPROM must report ErrNoTables so callers fall back to the env.
func TestParseBlankRegion(t *testing.T) {
	if _, err := Parse(make([]byte, testRegionSize)); !errors.Is(err, ErrNoTables) {
		t.Errorf("Parse(blank) = %v, want ErrNoTables", err)
	}
	if _, err := Parse(nil); !errors.Is(err, ErrNoTables) {
		t.Errorf("Parse(nil) = %v, want ErrNoTables", err)
	}
}

func TestParseRejectsBadChecksum(t *testing.T) {
	region := sampleRegion(t)
	region[5]++ // corrupt the entry point checksum
	if _, err := Parse(region); err == nil || errors.Is(err, ErrNoTables) {
		t.Errorf("Parse(bad checksum) = %v, want a checksum error", err)
	}
}

// memBackend serves a whole chip; the store must read only its region.
type memBackend struct{ buf []byte }

func (m *memBackend) Size() int { return len(m.buf) }

func (m *memBackend) ReadAt(off int, p []byte) error {
	if off < 0 || off+len(p) > len(m.buf) {
		return fmt.Errorf("read [%d,%d) out of range", off, off+len(p))
	}
	copy(p, m.buf[off:])
	return nil
}

// The store must read at CONFIG_SMBIOS_I2C_STORE_OFFSET, not offset 0 (which
// holds the UEFI variable blob).
func TestStoreReadsAtOffset(t *testing.T) {
	chip := &memBackend{buf: make([]byte, testChipSize)}
	// Seed the UEFI + env regions with noise the store must not parse.
	for i := range testOffset {
		chip.buf[i] = 0xAA
	}
	copy(chip.buf[testOffset:], sampleRegion(t))

	s := NewStore(chip, testOffset, testRegionSize)
	if !s.Available() {
		t.Fatal("store reports unavailable")
	}
	info, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Product != "Raspberry Pi 5 Model B Rev 1.1" {
		t.Errorf("Product = %q", info.Product)
	}
}

func TestStoreBlankAndUnconfigured(t *testing.T) {
	blank := NewStore(&memBackend{buf: make([]byte, testChipSize)}, testOffset, testRegionSize)
	if _, err := blank.Load(); !errors.Is(err, ErrNoTables) {
		t.Errorf("Load(blank) = %v, want ErrNoTables", err)
	}

	var nilStore *Store
	if nilStore.Available() {
		t.Error("nil store reports Available")
	}
	if _, err := nilStore.Load(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Load(nil) = %v, want ErrNotConfigured", err)
	}
}
