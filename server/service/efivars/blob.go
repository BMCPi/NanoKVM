package efivars

// blob.go encodes and decodes the serialized variable store blob written by
// U-Boot (lib/efi_loader/efi_var_i2c.c, format shared with ubootefi.var).
//
// Layout (all little-endian):
//
//	header (24 bytes):
//	  u64 reserved   — 0
//	  u64 magic      — 0x0161566966456255 ("UbEfiVa", version 1)
//	  u32 length     — total blob length including this header
//	  u32 crc32      — IEEE CRC32 of the payload (bytes [24, length))
//
//	entries, each starting on an 8-byte boundary:
//	  u32 length     — DATA length in bytes (not the entry length)
//	  u32 attr       — variable attributes
//	  u64 time       — authentication time (seconds since epoch)
//	  u8  guid[16]   — vendor GUID (binary EFI mixed-endian)
//	  u16 name[]     — UTF-16LE variable name, NUL-terminated
//	  u8  data[length]
//
// The next entry begins at ALIGN(offset_of_data + length, 8).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"unicode/utf16"
)

// Magic identifies the blob format ("UbEfiVa", version 1).
const Magic uint64 = 0x0161566966456255

const (
	headerSize     = 24
	entryHeaderLen = 32 // u32 length + u32 attr + u64 time + guid[16]
)

// ErrNoStore reports a blob without a valid magic — a blank or foreign
// EEPROM rather than a corrupted store.
var ErrNoStore = errors.New("efivars: no variable store found")

// ErrCorrupt reports a blob with a valid magic but inconsistent contents.
var ErrCorrupt = errors.New("efivars: variable store corrupted")

func align8(n int) int {
	return (n + 7) &^ 7
}

// DecodeHeader validates the fixed header and returns the total blob length.
func DecodeHeader(b []byte) (int, error) {
	if len(b) < headerSize {
		return 0, ErrNoStore
	}
	if binary.LittleEndian.Uint64(b[8:16]) != Magic {
		return 0, ErrNoStore
	}
	length := int(binary.LittleEndian.Uint32(b[16:20]))
	if length < headerSize {
		return 0, fmt.Errorf("%w: length %d below header size", ErrCorrupt, length)
	}
	return length, nil
}

// Decode parses a complete blob into variables, verifying magic and CRC32.
func Decode(b []byte) ([]Variable, error) {
	length, err := DecodeHeader(b)
	if err != nil {
		return nil, err
	}
	if length > len(b) {
		return nil, fmt.Errorf("%w: length %d exceeds blob size %d", ErrCorrupt, length, len(b))
	}
	wantCRC := binary.LittleEndian.Uint32(b[20:24])
	if got := crc32.ChecksumIEEE(b[headerSize:length]); got != wantCRC {
		return nil, fmt.Errorf("%w: CRC32 mismatch (stored %08x, computed %08x)", ErrCorrupt, wantCRC, got)
	}

	var vars []Variable
	off := headerSize
	for off+entryHeaderLen <= length {
		v, next, err := decodeEntry(b[:length], off)
		if err != nil {
			return nil, err
		}
		vars = append(vars, v)
		off = next
	}
	return vars, nil
}

func decodeEntry(b []byte, off int) (Variable, int, error) {
	var v Variable
	dataLen := int(binary.LittleEndian.Uint32(b[off : off+4]))
	v.Attributes = binary.LittleEndian.Uint32(b[off+4 : off+8])
	v.Time = binary.LittleEndian.Uint64(b[off+8 : off+16])
	copy(v.GUID[:], b[off+16:off+32])

	name, dataOff, err := decodeUTF16Name(b, off+entryHeaderLen)
	if err != nil {
		return v, 0, err
	}
	v.Name = name

	if dataOff+dataLen > len(b) {
		return v, 0, fmt.Errorf("%w: variable %q data exceeds blob", ErrCorrupt, name)
	}
	v.Data = append([]byte(nil), b[dataOff:dataOff+dataLen]...)
	return v, align8(dataOff + dataLen), nil
}

// decodeUTF16Name reads a NUL-terminated UTF-16LE string starting at off and
// returns the decoded name and the offset just past the terminator.
func decodeUTF16Name(b []byte, off int) (string, int, error) {
	var units []uint16
	for {
		if off+2 > len(b) {
			return "", 0, fmt.Errorf("%w: unterminated variable name", ErrCorrupt)
		}
		u := binary.LittleEndian.Uint16(b[off : off+2])
		off += 2
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units)), off, nil
}

// Encode serializes variables into a store blob, computing length and CRC32.
// Entry order follows the slice order.
func Encode(vars []Variable) []byte {
	size := headerSize
	for i := range vars {
		size = align8(size + entryHeaderLen + 2*(len(utf16.Encode([]rune(vars[i].Name)))+1) + len(vars[i].Data))
	}

	b := make([]byte, size)
	binary.LittleEndian.PutUint64(b[8:16], Magic)
	binary.LittleEndian.PutUint32(b[16:20], uint32(size))

	off := headerSize
	for i := range vars {
		off = encodeEntry(b, off, &vars[i])
	}
	binary.LittleEndian.PutUint32(b[20:24], crc32.ChecksumIEEE(b[headerSize:]))
	return b
}

func encodeEntry(b []byte, off int, v *Variable) int {
	binary.LittleEndian.PutUint32(b[off:off+4], uint32(len(v.Data))) //nolint:gosec // bounded
	binary.LittleEndian.PutUint32(b[off+4:off+8], v.Attributes)
	binary.LittleEndian.PutUint64(b[off+8:off+16], v.Time)
	copy(b[off+16:off+32], v.GUID[:])

	off += entryHeaderLen
	for _, u := range utf16.Encode([]rune(v.Name)) {
		binary.LittleEndian.PutUint16(b[off:off+2], u)
		off += 2
	}
	off += 2 // NUL terminator (buffer is zero-initialized)

	copy(b[off:], v.Data)
	return align8(off + len(v.Data))
}
