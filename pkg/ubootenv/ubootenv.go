// Package ubootenv parses and serializes U-Boot environment files.
//
// Two formats are supported and auto-detected on Parse:
//
//   - Text format (`env export -t`): one `key=value` pair per line, with
//     blank lines, `#` comments, and trailing-backslash continuation.
//     A trailing NUL byte is tolerated.
//
//   - Binary format (the on-disk image U-Boot reads from its env partition,
//     a.k.a. `machine.env`): a 4-byte little-endian CRC32 header followed by
//     null-terminated `key=value` entries, padded to a fixed size. The
//     environment is terminated by a double-NUL.
//
// SaveFile preserves the format of the loaded environment. Empty
// environments default to text; use NewBinary to create a binary env.
package ubootenv

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Format identifies the serialization used for an Env.
type Format int

const (
	// FormatText is U-Boot's `env export -t` plain-text format.
	FormatText Format = iota
	// FormatBinary is U-Boot's on-disk binary env partition format
	// (4-byte CRC32 LE header + NUL-terminated entries, padded to Size).
	FormatBinary
)

// DefaultEnvSize is the conventional total size of a U-Boot binary env
// partition (16 KiB). U-Boot's CONFIG_ENV_SIZE typically matches this.
const DefaultEnvSize = 0x4000

// crcSize is the size of the CRC32 header in the binary format.
const crcSize = 4

// Well-known U-Boot env variable names.
const (
	VarArch          = "arch"
	VarBaudRate      = "baudrate"
	VarBoard         = "board"
	VarBoardName     = "board_name"
	VarBoardRev      = "board_rev"
	VarBoardRevision = "board_revision"
	VarBootTargets   = "boot_targets"
	VarBootCmd       = "bootcmd"
	VarBootDelay     = "bootdelay"
	VarBootMeths     = "bootmeths"
	VarCPU           = "cpu"
	VarEthAddr       = "ethaddr"
	VarUSBEthAddr    = "usbethaddr"
	VarFDTFile       = "fdtfile"
	VarSerial        = "serial#"
	VarSOC           = "soc"
	VarVendor        = "vendor"
	VarVer           = "ver"
)

// inventoryKeys are env vars extracted by GetInventory.
var inventoryKeys = []string{
	VarArch,
	VarBoard,
	VarBoardName,
	VarBoardRev,
	VarBoardRevision,
	VarBootTargets,
	VarBootMeths,
	VarCPU,
	VarEthAddr,
	VarUSBEthAddr,
	VarFDTFile,
	VarSerial,
	VarSOC,
	VarVendor,
	VarVer,
}

// Env represents a parsed U-Boot environment.
type Env struct {
	Vars   map[string]string
	Format Format
	// Size is the total size of the underlying binary env partition,
	// including the CRC header. Only meaningful for FormatBinary.
	Size int
}

// New returns an empty text-format environment.
func New() *Env {
	return &Env{Vars: make(map[string]string), Format: FormatText}
}

// NewBinary returns an empty binary-format environment of the given total
// size (including the CRC32 header). If size <= 0, DefaultEnvSize is used.
func NewBinary(size int) *Env {
	if size <= 0 {
		size = DefaultEnvSize
	}
	return &Env{Vars: make(map[string]string), Format: FormatBinary, Size: size}
}

// LoadFile reads and parses a U-Boot environment file. The format is
// auto-detected from the file contents.
func LoadFile(path string) (*Env, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	return Parse(data)
}

// SaveFile serializes the environment and writes it atomically to the given
// path. The output format follows e.Format. The file is written to a
// temporary file in the same directory, then renamed to prevent corruption.
func (e *Env) SaveFile(path string) error {
	var data []byte
	var err error
	if e.Format == FormatBinary {
		data, err = e.MarshalBinary(e.Size)
		if err != nil {
			return err
		}
	} else {
		data = e.Marshal()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".machine.env.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp to env: %w", err)
	}
	return nil
}

// clone returns a deep copy, so callers cannot mutate a Store's cached env.
func (e *Env) clone() *Env {
	vars := make(map[string]string, len(e.Vars))
	for k, v := range e.Vars {
		vars[k] = v
	}
	return &Env{Vars: vars, Format: e.Format, Size: e.Size}
}

// Get returns the value of a variable and whether it exists.
func (e *Env) Get(key string) (string, bool) {
	v, ok := e.Vars[key]
	return v, ok
}

// Set sets a variable value. Creates the key if it doesn't exist.
func (e *Env) Set(key, value string) {
	if e.Vars == nil {
		e.Vars = make(map[string]string)
	}
	e.Vars[key] = value
}

// Delete removes a variable. No-op if the key doesn't exist.
func (e *Env) Delete(key string) {
	delete(e.Vars, key)
}

// GetBootTargets returns the space-separated boot_targets as a slice.
// Returns nil if boot_targets is not set.
func (e *Env) GetBootTargets() []string {
	v, ok := e.Vars[VarBootTargets]
	if !ok || v == "" {
		return nil
	}
	return strings.Fields(v)
}

// SetBootTargets sets boot_targets from a slice of target names.
// An empty slice deletes the variable.
func (e *Env) SetBootTargets(targets []string) {
	if len(targets) == 0 {
		delete(e.Vars, VarBootTargets)
		return
	}
	e.Set(VarBootTargets, strings.Join(targets, " "))
}

// GetInventory returns a map of well-known inventory variables and their
// values. Only variables that are present in the environment are included.
func (e *Env) GetInventory() map[string]string {
	inv := make(map[string]string)
	for _, key := range inventoryKeys {
		if v, ok := e.Vars[key]; ok {
			inv[key] = v
		}
	}
	return inv
}

// Parse auto-detects between binary and text formats and parses accordingly.
//
// Binary detection: if the data is at least crcSize+1 bytes and the CRC32
// header matches the remaining payload (with unused bytes treated as the
// terminator), it is parsed as binary. Otherwise it is parsed as text.
func Parse(data []byte) (*Env, error) {
	if env, ok := tryParseBinary(data); ok {
		return env, nil
	}
	return parseText(data)
}

// tryParseBinary attempts to parse data as a binary U-Boot env. Returns
// (env, true) on success, (nil, false) if the data does not match the
// binary format (in which case the caller should fall back to text).
func tryParseBinary(data []byte) (*Env, bool) {
	if len(data) < crcSize+1 {
		return nil, false
	}
	storedCRC := binary.LittleEndian.Uint32(data[:crcSize])
	payload := data[crcSize:]
	if storedCRC != crc32.ChecksumIEEE(payload) {
		return nil, false
	}

	vars := make(map[string]string)
	pos := 0
	for pos < len(payload) {
		if payload[pos] == 0 {
			break // end of environment
		}
		end := pos
		for end < len(payload) && payload[end] != 0 {
			end++
		}
		entry := string(payload[pos:end])
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			// Malformed binary entry — treat as a non-binary file.
			return nil, false
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, false
		}
		vars[k] = v
		pos = end + 1
	}

	return &Env{Vars: vars, Format: FormatBinary, Size: len(data)}, true
}

// parseText reads a U-Boot environment from the plain-text format produced
// by `env export -t`:
//
//   - one `key=value` pair per line
//   - blank lines and lines beginning with `#` are ignored
//   - a trailing backslash continues the value on the next line (the newline
//     itself becomes part of the value)
//   - a single trailing NUL byte (appended by U-Boot in memory) is tolerated
func parseText(data []byte) (*Env, error) {
	// `env export -t` output is plain text and never contains NUL bytes.
	// In practice the file may be padded with leftover cluster slack from
	// the FAT (or U-Boot may append a single NUL terminator in memory).
	// Truncate at the first NUL so trailing garbage is ignored.
	if i := bytes.IndexByte(data, 0); i >= 0 {
		data = data[:i]
	}

	vars := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Allow long lines (default is 64 KiB which is plenty, but be explicit).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		// Handle backslash continuation: a line ending in an unescaped `\`
		// joins with the following line, with a literal newline in between.
		for strings.HasSuffix(line, `\`) && !strings.HasSuffix(line, `\\`) {
			if !scanner.Scan() {
				break
			}
			lineNo++
			line = line[:len(line)-1] + "\n" + scanner.Text()
		}

		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("malformed entry on line %d: %q", lineNo, trimmed)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("empty key on line %d", lineNo)
		}
		vars[k] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan env: %w", err)
	}

	return &Env{Vars: vars, Format: FormatText}, nil
}

// Marshal serializes the environment to the plain text format. Keys are
// sorted for deterministic output. Values containing newlines are emitted
// using backslash continuation so they round-trip through Parse.
func (e *Env) Marshal() []byte {
	keys := make([]string, 0, len(e.Vars))
	for k := range e.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	for _, k := range keys {
		v := e.Vars[k]
		// Escape embedded newlines as backslash-continuation so Parse
		// reconstructs the exact value.
		v = strings.ReplaceAll(v, "\n", "\\\n")
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(v)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// MarshalBinary serializes the environment to U-Boot's binary partition
// format: a 4-byte CRC32 LE header followed by NUL-terminated `key=value`
// entries, padded with NUL bytes to total `size` bytes (including header).
// Keys are sorted for deterministic output.
//
// If size <= 0, e.Size is used; if that is also unset, DefaultEnvSize.
// Returns an error if the entries do not fit.
func (e *Env) MarshalBinary(size int) ([]byte, error) {
	if size <= 0 {
		size = e.Size
	}
	if size <= 0 {
		size = DefaultEnvSize
	}
	if size < crcSize+2 {
		return nil, fmt.Errorf("env size too small: %d", size)
	}

	buf := make([]byte, size)
	dataSize := size - crcSize

	keys := make([]string, 0, len(e.Vars))
	for k := range e.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pos := 0
	for _, k := range keys {
		entry := k + "=" + e.Vars[k]
		needed := len(entry) + 1 // +1 for null terminator
		// Reserve one extra byte for the final double-NUL terminator.
		if pos+needed+1 > dataSize {
			return nil, fmt.Errorf("environment data exceeds available space (%d bytes)", dataSize)
		}
		copy(buf[crcSize+pos:], entry)
		pos += len(entry)
		buf[crcSize+pos] = 0 // null terminator
		pos++
	}
	// Trailing region is already zero (double-NUL terminator + padding).

	payload := buf[crcSize:]
	checksum := crc32.ChecksumIEEE(payload)
	binary.LittleEndian.PutUint32(buf[:crcSize], checksum)

	return buf, nil
}
