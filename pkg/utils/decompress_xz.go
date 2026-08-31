package utils

// decompress_xz.go exists because xz.ReaderConfig.DictCap, despite its name,
// is not an upper bound in github.com/ulikunitz/xz (checked against v0.5.15
// and v0.5.16, the latest tagged release as of writing): the LZMA2 filter's
// reader takes max(our configured DictCap, the dictionary size declared in
// the stream's own block header), not min. A hostile stream's header can
// declare up to a 4 GiB dictionary in a single byte — independent of how
// much actual data follows — and the library allocates that eagerly, inside
// the first Read() call, before producing any output. On a ~256 MB device
// that is an immediate, unrecoverable process crash (Go's allocator treats
// an mmap it can't satisfy as fatal, not a catchable error), and it costs
// the attacker nothing: the compressed stream can be a few hundred bytes.
//
// So the declared dictionary size is checked against our cap by hand, over
// the same header bytes xz.NewReader will itself parse, before ever handing
// it a reader that might act on them. The parsing here mirrors (deliberately,
// byte for byte) the package's own unexported readBlockHeader/UnmarshalBinary
// — reusing its logic was not an option since none of it is exported — so
// that "did we read the same field the library will use" isn't a matter of
// re-deriving the xz spec from memory. lzma.DecodeDictCap, the one piece of
// that path this module does export, does the final byte-to-size decode.
//
// This only needs to handle what ulikunitz/xz itself can decode: its
// readFilters explicitly refuses any block header claiming more than one
// filter, and its readFilter refuses any filter but LZMA2. A header this
// parser can't positively verify (truncated, bad checksum, more than one
// filter, anything but LZMA2) is refused outright rather than let through on
// the assumption that the real decoder would also reject it — refusing a
// stream we can't parse is cheap; guessing wrong about one we can't parse is
// exactly the bug this file exists to avoid.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// Block header flag bits (xz-file-format 1.0.4 §3.1.3), named to match
// github.com/ulikunitz/xz's own (unexported) constants of the same purpose.
const (
	xzFilterCountMask         = 0x03
	xzCompressedSizePresent   = 0x40
	xzUncompressedSizePresent = 0x80
	xzReservedBlockFlags      = 0x3C

	xzLZMA2FilterID = 0x21 // the only filter ID ulikunitz/xz's decoder accepts
)

// xzDeclaredDictCap reads the LZMA2 dictionary capacity declared in the
// block header of an xz stream, given at least the stream header followed by
// the complete block header (head[xz.HeaderLen:]). It returns an error for
// anything it cannot fully verify, including a block header longer than the
// bytes supplied — callers should peek generously and treat that as "cannot
// verify" rather than grow the peek to chase it.
func xzDeclaredDictCap(head []byte) (int64, error) {
	if len(head) < xz.HeaderLen+2 {
		return 0, io.ErrUnexpectedEOF
	}
	block := head[xz.HeaderLen:]

	sizeByte := block[0]
	if sizeByte == 0 {
		return 0, errors.New("xz: block header size byte is the index indicator, not a block")
	}
	headerLen := (int(sizeByte) + 1) * 4
	if len(block) < headerLen {
		return 0, io.ErrUnexpectedEOF
	}
	block = block[:headerLen]

	// Verify the header's own CRC32 before trusting anything in it — the
	// same check xz.Reader performs. A dictionary size read from a header
	// that fails this check is not one the real decoder would have honored
	// either, so there's nothing to gain by parsing further.
	n := headerLen - 4
	if crc32.ChecksumIEEE(block[:n]) != binary.LittleEndian.Uint32(block[n:]) {
		return 0, errors.New("xz: block header checksum mismatch")
	}

	flags := block[1]
	if flags&xzReservedBlockFlags != 0 {
		return 0, errors.New("xz: reserved block header flags set")
	}
	if int(flags&xzFilterCountMask)+1 != 1 {
		// ulikunitz/xz's own readFilters refuses this too ("unsupported
		// filter count"); refusing it here as well just means we never
		// hand a header we haven't verified to code that might act on it.
		return 0, errors.New("xz: multiple filters not supported")
	}

	r := bytes.NewReader(block[2:n])
	if flags&xzCompressedSizePresent != 0 {
		if _, err := readXZVarint(r); err != nil {
			return 0, fmt.Errorf("xz: compressed size field: %w", err)
		}
	}
	if flags&xzUncompressedSizePresent != 0 {
		if _, err := readXZVarint(r); err != nil {
			return 0, fmt.Errorf("xz: uncompressed size field: %w", err)
		}
	}

	filterID, err := readXZVarint(r)
	if err != nil {
		return 0, fmt.Errorf("xz: filter id: %w", err)
	}
	if filterID != xzLZMA2FilterID {
		return 0, fmt.Errorf("xz: unsupported filter id %#x (only LZMA2 is)", filterID)
	}
	propsSize, err := readXZVarint(r)
	if err != nil {
		return 0, fmt.Errorf("xz: filter properties size: %w", err)
	}
	if propsSize != 1 {
		return 0, fmt.Errorf("xz: LZMA2 properties size %d, want 1", propsSize)
	}
	propsByte, err := r.ReadByte()
	if err != nil {
		return 0, fmt.Errorf("xz: filter properties byte: %w", err)
	}

	return lzma.DecodeDictCap(propsByte)
}

// readXZVarint reads one xz-format variable-length integer: little-endian,
// 7 payload bits per byte, high bit set means another byte follows. Mirrors
// github.com/ulikunitz/xz's own unexported readUvarint, including its
// 10-byte ceiling (ceil(64/7)), which is more than any field this parser
// reads ever legitimately needs.
func readXZVarint(r io.ByteReader) (uint64, error) {
	var x uint64
	var s uint
	for i := 0; i < 10; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b < 0x80 {
			return x | uint64(b)<<s, nil
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, errors.New("xz: varint too long")
}
