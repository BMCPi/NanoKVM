// Package efivars manages the UEFI variable store that U-Boot persists in
// an I2C EEPROM (CONFIG_EFI_VARIABLE_I2C_STORE on the host).
//
// The store is a single serialized blob — the same format as U-Boot's
// ubootefi.var file: a 24-byte header (reserved, magic, total length, CRC32
// of the payload) followed by 8-byte-aligned variable entries. Because the
// format is self-describing, the BMC can read and rewrite BootOrder,
// BootNext and friends out-of-band while the host is off or in U-Boot.
//
// Access paths (see Backend):
//   - a file: the backing file of a kernel i2c-slave-eeprom device when the
//     BMC emulates the EEPROM, an at24 sysfs "eeprom" node, or a plain file
//     for testing;
//   - a raw I2C bus (/dev/i2c-N) when the BMC masters a physical EEPROM.
package efivars

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Attribute bits (UEFI spec 2.x, SetVariable).
const (
	AttrNonVolatile                 = 0x00000001
	AttrBootserviceAccess           = 0x00000002
	AttrRuntimeAccess               = 0x00000004
	AttrTimeBasedAuthenticatedWrite = 0x00000020
	AttrReadOnly                    = 0x00000040 // U-Boot extension

	// AttrBootVariable is the standard attribute set for boot manager
	// variables (BootOrder, Boot####, BootNext).
	AttrBootVariable = AttrNonVolatile | AttrBootserviceAccess | AttrRuntimeAccess
)

// GUID is a binary EFI GUID (mixed-endian: the first three fields are
// little-endian, the final eight bytes are a byte array).
type GUID [16]byte

// EFIGlobalVariable is the vendor GUID of the standard boot manager
// variables (BootOrder, Boot####, BootNext, ...).
var EFIGlobalVariable = MustParseGUID("8be4df61-93ca-11d2-aa0d-00e098032b8c")

// String renders the GUID in canonical 8-4-4-4-12 text form.
func (g GUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.LittleEndian.Uint32(g[0:4]),
		binary.LittleEndian.Uint16(g[4:6]),
		binary.LittleEndian.Uint16(g[6:8]),
		binary.BigEndian.Uint16(g[8:10]),
		g[10:16])
}

// ParseGUID parses canonical 8-4-4-4-12 text form into a binary EFI GUID.
func ParseGUID(s string) (GUID, error) {
	var g GUID
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "-")
	if len(parts) != 5 ||
		len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 ||
		len(parts[3]) != 4 || len(parts[4]) != 12 {
		return g, fmt.Errorf("efivars: invalid GUID %q", s)
	}
	raw, err := hex.DecodeString(strings.Join(parts, ""))
	if err != nil {
		return g, fmt.Errorf("efivars: invalid GUID %q: %w", s, err)
	}
	binary.LittleEndian.PutUint32(g[0:4], binary.BigEndian.Uint32(raw[0:4]))
	binary.LittleEndian.PutUint16(g[4:6], binary.BigEndian.Uint16(raw[4:6]))
	binary.LittleEndian.PutUint16(g[6:8], binary.BigEndian.Uint16(raw[6:8]))
	copy(g[8:], raw[8:])
	return g, nil
}

// MustParseGUID is ParseGUID for compile-time constants; it panics on error.
func MustParseGUID(s string) GUID {
	g, err := ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

// Variable is one UEFI variable from the store.
type Variable struct {
	GUID       GUID
	Name       string
	Attributes uint32
	// Time is the authentication timestamp (seconds since epoch); zero for
	// non-authenticated variables. Preserved verbatim on round-trips.
	Time uint64
	Data []byte
}

// key returns the store lookup key (vendor GUID + name).
func (v *Variable) key() string {
	return v.GUID.String() + "/" + v.Name
}
