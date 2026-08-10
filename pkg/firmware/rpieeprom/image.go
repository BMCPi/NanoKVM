package rpieeprom

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Section magics ported from raspberrypi/rpi-eeprom (rpi-eeprom-config).
const (
	// Magic is the generic section magic. Bootcode (offset 0) uses it.
	Magic uint32 = 0x55aaf00f
	// PadMagic marks a padding section the updater inserts when a file
	// shrinks, to keep subsequent offsets stable.
	PadMagic uint32 = 0x55aafeef
	// FileMagic marks a modifiable file section (bootconf.txt, pubkey.bin…).
	FileMagic uint32 = 0x55aaf11f
	// MagicMask is used to validate that an unknown section magic is at
	// least shaped like one of the known section types.
	MagicMask uint32 = 0xfffff00f

	// FileHdrLen is the per-file header size: magic(4) + length(4) + 12-byte
	// null-padded filename. The bootcode section omits the filename, but the
	// length is still 4 bytes after the magic.
	FileHdrLen = 20
	// FilenameLen is the size of the embedded filename slot.
	FilenameLen = 12

	// EraseAlignSize is the size of one erase block and the size of the
	// trailing scratch area the bootloader reserves at the end of the image.
	EraseAlignSize = 4096
	// MaxFileSize is the largest payload a regular file section can hold.
	MaxFileSize = EraseAlignSize - FileHdrLen

	// ReadOnlySize is the size of the read-only header on AB-layout images
	// (bootcode lives here).
	ReadOnlySize = 64 * 1024
	// PartitionSize is the size of the writable partition on AB images.
	PartitionSize = 988 * 1024
)

// Well-known filenames carried inside the image.
const (
	BootCodeBin = "bootcode.bin"
	BootSysBin  = "bootsys"
	BootConfTxt = "bootconf.txt"
	BootConfSig = "bootconf.sig"
	PubKeyBin   = "pubkey.bin"
	CACertDer   = "cacert.der"
	UpdateTime  = "updatetime"
)

// ValidImageSizes lists the EEPROM image sizes the parser accepts.
// 512 KB images are RPi 4; 2 MB images are RPi 5 / AB-layout.
var ValidImageSizes = []int{512 * 1024, 2 * 1024 * 1024}

// Section describes one chunk found in the EEPROM image. Filename is empty
// for non-FileMagic sections.
type Section struct {
	Magic    uint32
	Offset   int
	Length   int
	Filename string
}

// Image holds an EEPROM image's bytes alongside its parsed section table.
// Methods that mutate (UpdateFile) operate directly on the in-memory bytes;
// call Write or Bytes to obtain the result.
type Image struct {
	data     []byte
	sections []Section
	ab       bool
}

// Parse reads the EEPROM image from r and returns a parsed Image. The full
// image is buffered in memory (max 2 MB). Returns an error for any image
// whose size isn't in ValidImageSizes or whose section table is corrupt.
func Parse(r io.Reader) (*Image, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes is like Parse but takes a byte slice directly. The slice is
// adopted; callers must not mutate it after handing it over.
func ParseBytes(data []byte) (*Image, error) {
	if !validImageSize(len(data)) {
		return nil, fmt.Errorf("invalid EEPROM image size %d bytes (expected one of %v)",
			len(data), ValidImageSizes)
	}
	im := &Image{data: data}
	if err := im.parse(); err != nil {
		return nil, err
	}
	return im, nil
}

// parse walks the section table from offset 0, recording each section's
// magic/offset/length and (for FileMagic) the embedded filename. Stops at
// the first 0x00 or 0xffffffff magic (EOF marker).
func (im *Image) parse() error {
	offset := 0
	for offset < len(im.data) {
		if offset+8 > len(im.data) {
			break
		}
		magic := binary.BigEndian.Uint32(im.data[offset:])
		length := int(binary.BigEndian.Uint32(im.data[offset+4:]))
		if magic == 0 || magic == 0xffffffff {
			break // EOF
		}
		if magic&MagicMask != Magic {
			return fmt.Errorf("corrupted EEPROM at offset %d: magic=%#x masked=%#x expected=%#x",
				offset, magic, magic&MagicMask, Magic)
		}
		filename := ""
		if magic == FileMagic {
			if offset+FileHdrLen > len(im.data) {
				return fmt.Errorf("truncated file header at offset %d", offset)
			}
			filename = strings.TrimRight(
				string(im.data[offset+8:offset+FileHdrLen]),
				"\x00",
			)
			if filename == BootSysBin {
				im.ab = true
			}
		}
		im.sections = append(im.sections, Section{
			Magic: magic, Offset: offset, Length: length, Filename: filename,
		})

		offset += 8 + length
		// Sections are 8-byte aligned.
		offset = (offset + 7) &^ 7
	}
	return nil
}

// Sections returns a copy of the parsed section table.
func (im *Image) Sections() []Section {
	out := make([]Section, len(im.sections))
	copy(out, im.sections)
	return out
}

// IsABImage reports whether the image is AB-layout (contains a `bootsys` file
// section). RPi 5 images are AB; RPi 4 single-partition images are not.
func (im *Image) IsABImage() bool { return im.ab }

// Size returns the image length in bytes.
func (im *Image) Size() int { return len(im.data) }

// Bytes returns a copy of the current (possibly updated) image bytes.
func (im *Image) Bytes() []byte {
	out := make([]byte, len(im.data))
	copy(out, im.data)
	return out
}

// Write writes the current image bytes to w.
func (im *Image) Write(w io.Writer) error {
	_, err := w.Write(im.data)
	return err
}

// GetFile returns the content bytes for the named file. BootCodeBin has a
// special layout (no embedded filename in the content); all other named
// files have content starting after a 4-byte length header + 12-byte
// filename.
func (im *Image) GetFile(filename string) ([]byte, error) {
	if filename == BootCodeBin {
		if len(im.sections) == 0 {
			return nil, fmt.Errorf("no sections in image")
		}
		s := im.sections[0]
		start := s.Offset + 8
		end := start + s.Length
		if end > len(im.data) {
			return nil, fmt.Errorf("bootcode section extends past image (start=%d end=%d image=%d)",
				start, end, len(im.data))
		}
		return cloneBytes(im.data[start:end]), nil
	}
	s, _, err := im.findFile(filename, -1, -1)
	if err != nil {
		return nil, err
	}
	start := s.Offset + 4 + FileHdrLen
	contentLen := s.Length - FilenameLen - 4
	if contentLen < 0 {
		return nil, fmt.Errorf("malformed file section %s: length=%d", filename, s.Length)
	}
	end := start + contentLen
	if end > len(im.data) {
		return nil, fmt.Errorf("file section %s extends past image", filename)
	}
	return cloneBytes(im.data[start:end]), nil
}

// UpdateFile replaces the content of the named file in the image. Returns
// an error if the file doesn't exist, the new content is too large for the
// file's section slot, or the section table is corrupt.
//
// After UpdateFile returns, the in-memory section table is *not* re-parsed
// — section offsets are unchanged. Call Reparse() if you need the table
// to reflect padding sections inserted by the update.
func (im *Image) UpdateFile(filename string, content []byte) error {
	bootcode := filename == BootCodeBin
	bootsys := filename == BootSysBin
	if !bootcode && !bootsys && len(content) > MaxFileSize {
		return fmt.Errorf("%s too large: %d bytes (max %d)", filename, len(content), MaxFileSize)
	}
	return im.update(filename, content, bootcode)
}

// Reparse rebuilds the section table from the current bytes. Cheap; useful
// after an UpdateFile if the caller wants to inspect padding sections that
// were just inserted.
func (im *Image) Reparse() error {
	im.sections = im.sections[:0]
	im.ab = false
	return im.parse()
}

// update mirrors BootloaderImage.update in the python reference. Bootcode
// is constrained to the read-only header [0, ReadOnlySize); regular files
// live in the writable partition [ReadOnlySize, ReadOnlySize+PartitionSize).
func (im *Image) update(filename string, content []byte, bootcode bool) error {
	if bootcode {
		return im.updateBootcode(filename, content)
	}

	s, idx, err := im.findFile(filename, ReadOnlySize, ReadOnlySize+PartitionSize)
	if err != nil {
		return fmt.Errorf("find %s: %w", filename, err)
	}
	isLast := idx == len(im.sections)-1

	updateLen := len(content) + FileHdrLen
	if s.Offset+updateLen > len(im.data)-EraseAlignSize {
		return fmt.Errorf("no space for %s: need %d bytes, only %d available before reserved scratch",
			filename, updateLen, len(im.data)-EraseAlignSize-s.Offset)
	}

	nextOffset := im.findNextOffset(idx)
	if s.Offset+updateLen > nextOffset {
		return fmt.Errorf("update %d bytes is larger than section slot (%d bytes)",
			updateLen, nextOffset-s.Offset)
	}

	// Header layout after write: [magic][length=content+filename+4][filename(12)][content]
	// Per python: `new_len = len(src_bytes) + FILENAME_LEN + 4`. The +4
	// accounts for one extra word the bootloader uses internally; the magic
	// is left untouched (already FileMagic) and the filename bytes already
	// hold the right name (we matched by filename to find this section).
	newLen := len(content) + FilenameLen + 4
	binary.BigEndian.PutUint32(im.data[s.Offset+4:], uint32(newLen))
	copy(im.data[s.Offset+4+FileHdrLen:s.Offset+4+FileHdrLen+len(content)], content)

	padStart := s.Offset + 4 + FileHdrLen + len(content)
	return im.padTo(padStart, nextOffset, isLast)
}

// updateBootcode handles the special-case bootcode.bin layout: content sits
// directly after a 4-byte length header, with no embedded filename.
func (im *Image) updateBootcode(filename string, content []byte) error {
	s, idx, err := im.findFile(filename, 0, ReadOnlySize)
	if err != nil {
		return fmt.Errorf("find bootcode: %w", err)
	}

	if s.Offset+8+len(content) > len(im.data) {
		return fmt.Errorf("bootcode write past image end")
	}

	binary.BigEndian.PutUint32(im.data[s.Offset+4:], uint32(len(content)))
	copy(im.data[s.Offset+8:s.Offset+8+len(content)], content)

	nextOffset := im.findNextOffset(idx)
	if !im.ab && nextOffset < 128*1024 {
		return fmt.Errorf("update-bootcode: 128K must be reserved for bootcode (next=%d)", nextOffset)
	}
	if nextOffset < 0 {
		return fmt.Errorf("update-bootcode: failed to find next section")
	}
	return im.padTo(s.Offset+8+len(content), nextOffset, false /* never the last section */)
}

// padTo fills [padStart, nextOffset) with 0xff bytes. If the gap is at
// least 8 bytes and the section being updated isn't the last in the image,
// a PAD_MAGIC header is injected so subsequent parses walk past the gap.
func (im *Image) padTo(padStart, nextOffset int, isLast bool) error {
	// Round padStart up to the next 8-byte boundary (filling with 0xff).
	for padStart%8 != 0 {
		if padStart >= len(im.data) {
			return fmt.Errorf("pad alignment ran past image end")
		}
		im.data[padStart] = 0xff
		padStart++
	}
	padBytes := nextOffset - padStart
	if padBytes < 0 {
		return fmt.Errorf("negative padding %d (padStart=%d nextOffset=%d)",
			padBytes, padStart, nextOffset)
	}
	if padBytes >= 8 && !isLast {
		padBytes -= 8
		binary.BigEndian.PutUint32(im.data[padStart:], PadMagic)
		binary.BigEndian.PutUint32(im.data[padStart+4:], uint32(padBytes))
		padStart += 8
	}
	if padStart+padBytes > len(im.data) {
		return fmt.Errorf("padding extends past image end")
	}
	for i := 0; i < padBytes; i++ {
		im.data[padStart+i] = 0xff
	}
	return nil
}

// findFile locates a FileMagic section by filename, optionally restricted
// to [partStart, partEnd). partStart=-1 or partEnd=-1 disable the bound.
// Returns the section and its index in im.sections.
func (im *Image) findFile(filename string, partStart, partEnd int) (Section, int, error) {
	for i, s := range im.sections {
		if s.Magic != FileMagic || s.Filename != filename {
			continue
		}
		if partStart >= 0 && s.Offset < partStart {
			continue
		}
		if partEnd >= 0 && s.Offset >= partEnd {
			continue
		}
		return s, i, nil
	}
	return Section{}, -1, fmt.Errorf("file %q not found", filename)
}

// findNextOffset walks forward from idx, skipping PadMagic sections, to
// find the next "real" section's offset. Falls back to the start of the
// reserved trailer when no later non-pad section exists.
func (im *Image) findNextOffset(idx int) int {
	next := len(im.data) - EraseAlignSize
	for j := idx + 1; j < len(im.sections); j++ {
		if im.sections[j].Magic == PadMagic {
			continue
		}
		next = im.sections[j].Offset
		break
	}
	return next
}

// buildTimestampMarker is the ASCII tag the upstream rpi-eeprom build
// embeds inside bootcode so tools like `rpi-eeprom-update` can read the
// image version without a separate metadata file. Value is the Unix
// seconds at build time, ASCII-decimal-encoded.
var buildTimestampMarker = []byte("BUILD_TIMESTAMP=")

// Version returns the bootloader build timestamp embedded in the image,
// or the zero time and ok=false when no marker is found. The marker is
// searched only in the bootcode section (the read-only header on AB
// images) — that's where rpi-eeprom puts it.
func (im *Image) Version() (time.Time, bool) {
	bootcode, err := im.GetFile(BootCodeBin)
	if err != nil {
		return time.Time{}, false
	}
	idx := bytes.Index(bootcode, buildTimestampMarker)
	if idx < 0 {
		return time.Time{}, false
	}
	start := idx + len(buildTimestampMarker)
	end := start
	for end < len(bootcode) && bootcode[end] >= '0' && bootcode[end] <= '9' {
		end++
	}
	if end == start {
		return time.Time{}, false
	}
	ts, err := strconv.ParseInt(string(bootcode[start:end]), 10, 64)
	if err != nil || ts <= 0 {
		return time.Time{}, false
	}
	return time.Unix(ts, 0).UTC(), true
}

// SetTimestamp writes the self-update timestamp into the image. The Pi
// bootloader's SELF-UPDATE path compares the .upd's `updatetime` against
// the running EEPROM's; if the .upd's value is not strictly greater the
// update is skipped (with "SELF-UPDATE timestamp current N new M skip"
// in the early-stage log). Upstream `rpi-eeprom-update` always sets this
// to `date -u +%s` when staging — we do the same.
//
// Layout matches `rpi-eeprom-config.set_timestamp`: each `updatetime`
// file section's 8-byte content holds ~ts at offset 0 and ts at offset 4
// (little-endian uint32s). The XOR pair is a corruption check. There are
// two sections on AB images — one in the read-only header [0, ReadOnlySize)
// and one in the partition [ReadOnlySize, ReadOnlySize+PartitionSize) —
// and both must be updated.
func (im *Image) SetTimestamp(ts int64) error {
	tsLo := uint32(ts)
	inv := ^tsLo

	write := func(sec Section) {
		contentOff := sec.Offset + 4 + FileHdrLen
		binary.LittleEndian.PutUint32(im.data[contentOff:], inv)
		binary.LittleEndian.PutUint32(im.data[contentOff+4:], tsLo)
	}

	if s, _, err := im.findFile(UpdateTime, 0, ReadOnlySize); err == nil {
		write(s)
	} else if im.ab {
		// AB images must carry the RO updatetime; missing it would mean
		// our parse is wrong or the image is malformed.
		return fmt.Errorf("set timestamp: read-only updatetime missing: %w", err)
	}

	s, _, err := im.findFile(UpdateTime, ReadOnlySize, ReadOnlySize+PartitionSize)
	if err != nil {
		if im.ab {
			return fmt.Errorf("set timestamp: partition updatetime missing: %w", err)
		}
		// Non-AB (RPi 4 512K) images don't have the partition copy.
		return nil
	}
	write(s)
	return nil
}

func validImageSize(n int) bool {
	return slices.Contains(ValidImageSizes, n)
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
