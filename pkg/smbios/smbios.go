// Package smbios reads the SMBIOS tables that U-Boot mirrors into the I2C
// EEPROM, and renders them as host inventory.
//
// U-Boot (CONFIG_SMBIOS_I2C_STORE) writes the tables it generates at boot into
// a region of the same 24c256 that carries the UEFI variable store and the
// environment:
//
//	0x0000..0x3fff  UEFI variable blob (efivars)
//	0x4000..0x5fff  U-Boot environment (ubootenv)
//	0x6000..0x67ff  SMBIOS tables      (this package)
//
// The blob is exactly what U-Boot generated: a 24-byte _SM3_ entry point
// followed - 16-byte aligned - by the structure table. These are the same
// bytes the host OS sees, and they carry strictly more than the environment
// does: the type 1 UUID, the real product name and version, SKU, and the
// processor/cache detail exist nowhere else. So the BMC prefers this over the
// environment for inventory.
//
// Unlike efivars/ubootenv this store is read-only here - only the host writes
// it - so there is no snapshot to reconcile. A blank region simply reports
// ErrNoTables and callers fall back to the environment.
package smbios

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	gosmbios "github.com/siderolabs/go-smbios/smbios"
)

// Backend reads the raw bytes backing the store using absolute offsets into
// the device. The efivars file and I2C backends satisfy it structurally, so
// every store shares one EEPROM device without this package depending on them.
type Backend interface {
	ReadAt(off int, p []byte) error
	Size() int
}

const (
	// cacheTTL bounds how long parsed tables are served without re-reading
	// the EEPROM. The host only rewrites them across a U-Boot update or a
	// serial#/DT change, so this can be generous.
	cacheTTL = 30 * time.Second

	// entryPointLen is sizeof(struct smbios3_entry) in U-Boot.
	entryPointLen = 24

	// tableAlign mirrors write_smbios_table(), which starts the structure
	// table at ALIGN(addr + sizeof(struct smbios3_entry), 16).
	tableAlign = 16
)

var (
	// ErrNotConfigured is returned by a Store with no backend wired up.
	ErrNotConfigured = errors.New("smbios: store not configured")
	// ErrNoTables is returned when the region holds no valid entry point -
	// a blank EEPROM, or a host that has not booted this firmware yet.
	ErrNoTables = errors.New("smbios: no tables in the store")

	anchor = [5]byte{'_', 'S', 'M', '3', '_'}
)

// entryPoint mirrors U-Boot's packed struct smbios3_entry (little-endian).
type entryPoint struct {
	Anchor       [5]byte
	Checksum     uint8
	Length       uint8
	MajorVer     uint8
	MinorVer     uint8
	DocRev       uint8
	Revision     uint8
	Reserved     uint8
	TableMaxSize uint32
	TableAddress uint64
}

// Info is the host inventory derived from the tables. Fields are empty when
// the corresponding structure or string is absent.
type Info struct {
	// Type 1 - System Information.
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Version      string `json:"version,omitempty"`
	Serial       string `json:"serial,omitempty"`
	UUID         string `json:"uuid,omitempty"`
	SKU          string `json:"sku,omitempty"`
	Family       string `json:"family,omitempty"`

	// Type 0 - BIOS Information.
	BIOSVendor  string `json:"biosVendor,omitempty"`
	BIOSVersion string `json:"biosVersion,omitempty"`
	BIOSDate    string `json:"biosDate,omitempty"`
	// BootFirmwareVersion is a convenience view of the first FirmwareInventory
	// entry's Version — on this platform the rpi-eeprom boot firmware, as an
	// ISO 8601 date "YYYY-MM-DD". Empty when the host reports no Type 45.
	BootFirmwareVersion string `json:"bootFirmwareVersion,omitempty"`

	// Type 45 - Firmware Inventory Information. The firmware images the host
	// reports; the rpi-eeprom bootloader is the one this platform publishes.
	FirmwareInventory []FirmwareComponent `json:"firmwareInventory,omitempty"`

	// Type 2 - Baseboard Information.
	BoardManufacturer string `json:"boardManufacturer,omitempty"`
	BoardProduct      string `json:"boardProduct,omitempty"`
	BoardSerial       string `json:"boardSerial,omitempty"`

	// Type 4 - Processor Information (first socket).
	CPUManufacturer string `json:"cpuManufacturer,omitempty"`
	CPUVersion      string `json:"cpuVersion,omitempty"`
	CPUPartNumber   string `json:"cpuPartNumber,omitempty"`
	CPUCores        int    `json:"cpuCores,omitempty"`
	CPUThreads      int    `json:"cpuThreads,omitempty"`
	CPUMaxSpeedMHz  int    `json:"cpuMaxSpeedMHz,omitempty"`

	// Type 16 - Physical Memory Array (the aggregate the devices belong to).
	// MemoryErrorCorrection is "" when the array reports the SMBIOS placeholder
	// values (Unknown/Other), which carry no inventory value.
	MemoryErrorCorrection string `json:"memoryErrorCorrection,omitempty"`
	MemorySlots           int    `json:"memorySlots,omitempty"`

	// Type 17 - Memory Devices. MemoryTotalMB is the sum of the installed
	// modules; Memory carries the per-module detail.
	MemoryTotalMB int            `json:"memoryTotalMB,omitempty"`
	Memory        []MemoryModule `json:"memory,omitempty"`

	// Type 9 - System Slots (designations only; go-smbios exposes no more).
	Slots []string `json:"slots,omitempty"`

	// SMBIOSVersion is the spec version the host reported, e.g. "3.7.0".
	SMBIOSVersion string `json:"smbiosVersion,omitempty"`
}

// FirmwareComponent is one Type 45 Firmware Inventory Information structure —
// a firmware image the host carries. Unlike the byte-wide release fields of
// Type 0, every identifying field here is a free-form string, so a component
// versioned by date or by a git hash is reported exactly.
type FirmwareComponent struct {
	// Name is the component name, e.g. "Raspberry Pi Boot Firmware".
	Name string `json:"name,omitempty"`
	// Version is RawVersion normalised: an ISO date "YYYY-MM-DD" when the
	// firmware is date-versioned in the "YYYY.MMDD" form U-Boot writes for
	// the rpi-eeprom, otherwise the raw string unchanged.
	Version string `json:"version,omitempty"`
	// RawVersion is the version exactly as SMBIOS carries it, e.g. "2026.525".
	RawVersion string `json:"rawVersion,omitempty"`
	// ID identifies the image — the rpi-eeprom git hash on this platform.
	ID string `json:"id,omitempty"`
	// ReleaseDate is the component's release date as the host reported it.
	ReleaseDate string `json:"releaseDate,omitempty"`
	// Manufacturer of the firmware image.
	Manufacturer string `json:"manufacturer,omitempty"`
}

// MemoryModule is one populated Type 17 Memory Device. Fields are omitted when
// the device leaves them blank or reports an SMBIOS "Unknown"/"Other"
// placeholder, so an unpopulated attribute never shows up as a misleading zero
// or literal "Unknown". JSON tags are PascalCase because the only place these
// surface is a Redfish Oem block (see redfish/inventory.go).
type MemoryModule struct {
	Locator            string `json:"Locator,omitempty"`
	BankLocator        string `json:"BankLocator,omitempty"`
	SizeMB             int    `json:"SizeMB,omitempty"`
	Type               string `json:"Type,omitempty"`
	FormFactor         string `json:"FormFactor,omitempty"`
	SpeedMTs           int    `json:"SpeedMTs,omitempty"`
	ConfiguredSpeedMTs int    `json:"ConfiguredSpeedMTs,omitempty"`
	Manufacturer       string `json:"Manufacturer,omitempty"`
	PartNumber         string `json:"PartNumber,omitempty"`
	SerialNumber       string `json:"SerialNumber,omitempty"`
	AssetTag           string `json:"AssetTag,omitempty"`
	DataWidthBits      int    `json:"DataWidthBits,omitempty"`
	TotalWidthBits     int    `json:"TotalWidthBits,omitempty"`
}

// Store reads the SMBIOS tables from a fixed [offset, offset+size) region of
// an EEPROM. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	backend Backend
	offset  int
	size    int

	cache     *Info
	cacheTime time.Time
}

// NewStore returns a Store over the given EEPROM region.
func NewStore(b Backend, offset, size int) *Store {
	return &Store{backend: b, offset: offset, size: size}
}

// Available reports whether a backend is configured.
func (s *Store) Available() bool { return s != nil && s.backend != nil }

// Invalidate drops the cached tables, forcing the next read to hit the EEPROM.
func (s *Store) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheTime = time.Time{}
}

// Load returns the parsed inventory. It reports ErrNoTables when the region is
// blank or holds no valid entry point, so callers can fall back.
func (s *Store) Load() (*Info, error) {
	if !s.Available() {
		return nil, ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil && !s.cacheTime.IsZero() && time.Since(s.cacheTime) < cacheTTL {
		return s.cache, nil
	}

	raw := make([]byte, s.size)
	if err := s.backend.ReadAt(s.offset, raw); err != nil {
		return nil, fmt.Errorf("smbios: read region at %#x: %w", s.offset, err)
	}

	info, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	s.cache, s.cacheTime = info, time.Now()
	return info, nil
}

// Parse decodes a raw region: the _SM3_ entry point followed by the structure
// table. Exported so the region can be parsed from a file or a test fixture.
func Parse(raw []byte) (*Info, error) {
	if len(raw) < entryPointLen {
		return nil, ErrNoTables
	}

	var ep entryPoint
	if err := binary.Read(bytes.NewReader(raw[:entryPointLen]), binary.LittleEndian, &ep); err != nil {
		return nil, fmt.Errorf("smbios: decode entry point: %w", err)
	}
	if ep.Anchor != anchor {
		return nil, ErrNoTables
	}
	if ep.Length < entryPointLen || int(ep.Length) > len(raw) {
		return nil, fmt.Errorf("smbios: bogus entry point length %d", ep.Length)
	}
	if !checksumOK(raw[:ep.Length]) {
		return nil, errors.New("smbios: entry point checksum mismatch")
	}

	// The tables follow the entry point, 16-byte aligned, exactly as
	// write_smbios_table() lays them out. The entry point's TableAddress is
	// the DRAM address they were generated at and is meaningless here.
	start := align(int(ep.Length), tableAlign)
	end := start + int(ep.TableMaxSize)
	if ep.TableMaxSize == 0 || end > len(raw) {
		return nil, fmt.Errorf("smbios: table of %d bytes does not fit the %d-byte region",
			ep.TableMaxSize, len(raw))
	}

	version := gosmbios.Version{
		Major:    int(ep.MajorVer),
		Minor:    int(ep.MinorVer),
		Revision: int(ep.DocRev),
	}
	s, err := gosmbios.Decode(bytes.NewReader(raw[start:end]), version)
	if err != nil {
		return nil, fmt.Errorf("smbios: decode tables: %w", err)
	}

	return infoFrom(s, version), nil
}

func infoFrom(s *gosmbios.SMBIOS, v gosmbios.Version) *Info {
	info := &Info{
		SMBIOSVersion: fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Revision),

		Manufacturer: s.SystemInformation.Manufacturer,
		Product:      s.SystemInformation.ProductName,
		Version:      s.SystemInformation.Version,
		Serial:       s.SystemInformation.SerialNumber,
		UUID:         s.SystemInformation.UUID,
		SKU:          s.SystemInformation.SKUNumber,
		Family:       s.SystemInformation.Family,

		BIOSVendor:  s.BIOSInformation.Vendor,
		BIOSVersion: s.BIOSInformation.Version,
		BIOSDate:    s.BIOSInformation.ReleaseDate,

		BoardManufacturer: s.BaseboardInformation.Manufacturer,
		BoardProduct:      s.BaseboardInformation.Product,
		BoardSerial:       s.BaseboardInformation.SerialNumber,
	}

	if len(s.ProcessorInformation) > 0 {
		p := s.ProcessorInformation[0]
		info.CPUManufacturer = p.ProcessorManufacturer
		info.CPUVersion = p.ProcessorVersion
		info.CPUPartNumber = p.PartNumber
		info.CPUCores = int(p.CoreCount)
		info.CPUThreads = int(p.ThreadCount)
		info.CPUMaxSpeedMHz = int(p.MaxSpeed)
	}

	// Type 17 - Memory Devices. Sum the installed sizes for the summary and
	// keep the per-module detail. An empty socket (size 0) or an unknown size
	// contributes nothing and carries no identity worth reporting.
	//
	// The enum fields (type, form factor) are decoded from the raw SMBIOS byte
	// rather than through go-smbios's String() methods — see the decoder
	// helpers for why those cannot be trusted here.
	for i := range s.MemoryDevices {
		d := &s.MemoryDevices[i]
		mb := memoryDeviceMB(d)
		if mb == 0 {
			continue
		}
		info.MemoryTotalMB += mb
		info.Memory = append(info.Memory, MemoryModule{
			Locator:            d.DeviceLocator,
			BankLocator:        d.BankLocator,
			SizeMB:             mb,
			Type:               memoryTypeName(int(d.MemoryType)),
			FormFactor:         formFactorName(int(d.FormFactor)),
			SpeedMTs:           speedMTs(d.Speed),
			ConfiguredSpeedMTs: speedMTs(d.ConfiguredMemorySpeed),
			Manufacturer:       d.Manufacturer,
			PartNumber:         d.PartNumber,
			SerialNumber:       d.SerialNumber,
			AssetTag:           d.AssetTag,
			DataWidthBits:      widthBits(d.DataWidth),
			TotalWidthBits:     widthBits(d.TotalWidth),
		})
	}

	// Type 16 - Physical Memory Array. go-smbios zero-values this struct when
	// no type 16 is present, so only trust it alongside the type 17 devices it
	// aggregates.
	if len(s.MemoryDevices) > 0 {
		info.MemoryErrorCorrection = memoryErrorCorrectionName(int(s.PhysicalMemoryArray.MemoryErrorCorrection))
		info.MemorySlots = int(s.PhysicalMemoryArray.NumberOfMemoryDevices)
	}

	// Type 9 - System Slots.
	for i := range s.SystemSlots {
		if d := s.SystemSlots[i].SlotDesignation; d != "" {
			info.Slots = append(info.Slots, d)
		}
	}

	// Type 45 - Firmware Inventory. This platform publishes exactly one
	// component (the rpi-eeprom bootloader), so the first entry backs the
	// BootFirmwareVersion convenience field.
	info.FirmwareInventory = firmwareInventory(s)
	if len(info.FirmwareInventory) > 0 {
		info.BootFirmwareVersion = info.FirmwareInventory[0].Version
	}

	return info
}

// firmwareInventory decodes the Type 45 Firmware Inventory Information
// structures. go-smbios has no typed accessor for Type 45, so read the raw
// structure: Formatted begins at offset 0x04, and each string field holds a
// 1-based index into the structure's string set (0 meaning "no string").
func firmwareInventory(s *gosmbios.SMBIOS) []FirmwareComponent {
	// Offsets within Formatted, i.e. the SMBIOS offset less the 4-byte header.
	const (
		typeFirmwareInventory = 45
		offComponentName      = 0x04 - 0x04
		offVersion            = 0x05 - 0x04
		offFirmwareID         = 0x07 - 0x04
		offReleaseDate        = 0x09 - 0x04
		offManufacturer       = 0x0a - 0x04
	)

	var out []FirmwareComponent
	for _, st := range s.Structures {
		if st == nil || st.Header.Type != typeFirmwareInventory {
			continue
		}
		// Too short to carry the fields we read; skip rather than panic.
		if len(st.Formatted) <= offManufacturer {
			continue
		}

		raw := smbiosString(st.Strings, st.Formatted[offVersion])
		version := isoFromCompactDate(raw)
		if version == "" {
			version = raw // not date-versioned; report it unchanged
		}

		out = append(out, FirmwareComponent{
			Name:         smbiosString(st.Strings, st.Formatted[offComponentName]),
			Version:      version,
			RawVersion:   raw,
			ID:           smbiosString(st.Strings, st.Formatted[offFirmwareID]),
			ReleaseDate:  smbiosString(st.Strings, st.Formatted[offReleaseDate]),
			Manufacturer: smbiosString(st.Strings, st.Formatted[offManufacturer]),
		})
	}
	return out
}

// smbiosString resolves an SMBIOS string reference: a 1-based index into the
// structure's string set, where 0 means the field carries no string.
func smbiosString(strs []string, ref byte) string {
	if ref == 0 || int(ref) > len(strs) {
		return ""
	}
	return strings.TrimSpace(strs[ref-1])
}

// isoFromCompactDate converts the date-versioned "YYYY.MMDD" form U-Boot writes
// for the rpi-eeprom — the year, then the month with the day as the right two
// digits ("2026.525", "2026.1202") — into an ISO date "YYYY-MM-DD". Returns ""
// when v is not that form, so firmware versioned some other way keeps its raw
// string. The round-trip through time.Parse rejects impossible dates (02/30).
func isoFromCompactDate(v string) string {
	yStr, mdStr, ok := strings.Cut(v, ".")
	if !ok {
		return ""
	}
	year, err := strconv.Atoi(yStr)
	if err != nil {
		return ""
	}
	md, err := strconv.Atoi(mdStr)
	if err != nil {
		return ""
	}

	t, err := time.Parse("2006-1-2", fmt.Sprintf("%d-%d-%d", year, md/100, md%100))
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// memoryDeviceMB returns a memory device's installed size in megabytes, or 0
// for an empty socket or an unknown size. It mirrors the SMBIOS size encoding:
// bit 15 of Size selects KB vs MB (go-smbios's Megabytes handles that), and
// 0x7FFF defers to the 32-bit Extended Size, which the spec expresses in
// megabytes.
func memoryDeviceMB(d *gosmbios.MemoryDevice) int {
	switch d.Size {
	case 0, 0xFFFF:
		return 0
	case 0x7FFF:
		return int(d.ExtendedSize)
	default:
		return d.Size.Megabytes()
	}
}

// speedMTs returns a memory speed in MT/s, or 0 when the device reports the
// SMBIOS unknown (0) or "see extended speed" (0xFFFF) sentinels.
func speedMTs(s gosmbios.MemoryDeviceSpeed) int {
	if s == 0 || s == 0xFFFF {
		return 0
	}
	return int(s)
}

// widthBits returns a memory device data/total width in bits, or 0 for the
// SMBIOS unknown sentinel (0xFFFF).
func widthBits(w gosmbios.MemoryDeviceWidth) int {
	if w == 0xFFFF {
		return 0
	}
	return int(w)
}

// The three enum decoders below intentionally do NOT go through go-smbios's
// String() methods. go-smbios v0.3.4 numbers its enum constants from zero,
// while SMBIOS (DSP0134) numbers these enumerations from 0x01 and skips
// reserved ranges. Decoding the spec-compliant bytes U-Boot writes through
// go-smbios therefore yields values shifted by one or more — a real LPDDR4
// module reads back as "HBM2", a "Row of chips" package as "RIMM", and ECC
// "Unknown" as "None". We map the raw bytes ourselves, and return "" for the
// Other/Unknown/Reserved placeholders so they never surface as inventory.

// memoryTypeName maps an SMBIOS Memory Device "Memory Type" byte (DSP0134
// 7.18.2) to a name. The spellings match the Redfish MemoryDeviceType enum
// where the two overlap (e.g. "DDR4", "LPDDR4").
func memoryTypeName(b int) string {
	switch b {
	case 0x03:
		return "DRAM"
	case 0x04:
		return "EDRAM"
	case 0x05:
		return "VRAM"
	case 0x06:
		return "SRAM"
	case 0x07:
		return "RAM"
	case 0x08:
		return "ROM"
	case 0x09:
		return "FLASH"
	case 0x0A:
		return "EEPROM"
	case 0x0B:
		return "FEPROM"
	case 0x0C:
		return "EPROM"
	case 0x0D:
		return "CDRAM"
	case 0x0E:
		return "3DRAM"
	case 0x0F:
		return "SDRAM"
	case 0x10:
		return "SGRAM"
	case 0x11:
		return "RDRAM"
	case 0x12:
		return "DDR"
	case 0x13:
		return "DDR2"
	case 0x14:
		return "DDR2 FB-DIMM"
	case 0x18:
		return "DDR3"
	case 0x19:
		return "FBD2"
	case 0x1A:
		return "DDR4"
	case 0x1B:
		return "LPDDR"
	case 0x1C:
		return "LPDDR2"
	case 0x1D:
		return "LPDDR3"
	case 0x1E:
		return "LPDDR4"
	case 0x1F:
		return "Logical non-volatile device"
	case 0x20:
		return "HBM"
	case 0x21:
		return "HBM2"
	case 0x22:
		return "DDR5"
	case 0x23:
		return "LPDDR5"
	default: // 0x01 Other, 0x02 Unknown, 0x15-0x17 Reserved, unknown
		return ""
	}
}

// formFactorName maps an SMBIOS Memory Device "Form Factor" byte (DSP0134
// 7.18.1) to a name. The RPi 5's soldered package reports "Row of chips".
func formFactorName(b int) string {
	switch b {
	case 0x03:
		return "SIMM"
	case 0x04:
		return "SIP"
	case 0x05:
		return "Chip"
	case 0x06:
		return "DIP"
	case 0x07:
		return "ZIP"
	case 0x08:
		return "Proprietary Card"
	case 0x09:
		return "DIMM"
	case 0x0A:
		return "TSOP"
	case 0x0B:
		return "Row of chips"
	case 0x0C:
		return "RIMM"
	case 0x0D:
		return "SODIMM"
	case 0x0E:
		return "SRIMM"
	case 0x0F:
		return "FB-DIMM"
	case 0x10:
		return "Die"
	default: // 0x01 Other, 0x02 Unknown, unknown
		return ""
	}
}

// memoryErrorCorrectionName maps an SMBIOS Physical Memory Array "Memory Error
// Correction" byte (DSP0134 7.17.3) to a name.
func memoryErrorCorrectionName(b int) string {
	switch b {
	case 0x03:
		return "None"
	case 0x04:
		return "Parity"
	case 0x05:
		return "Single-bit ECC"
	case 0x06:
		return "Multi-bit ECC"
	case 0x07:
		return "CRC"
	default: // 0x01 Other, 0x02 Unknown, unknown
		return ""
	}
}

// checksumOK verifies the entry point's 8-bit sum-to-zero checksum.
func checksumOK(b []byte) bool {
	var sum uint8
	for _, c := range b {
		sum += c
	}
	return sum == 0
}

func align(n, a int) int { return (n + a - 1) &^ (a - 1) }
