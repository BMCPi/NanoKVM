package streamio

// decompress.go lets virtual-media uploads and fetches accept a compressed
// image and stage the decompressed bytes, without ever writing the
// intermediate compressed stream to disk or buffering it in memory: the
// decoder sits inline in the same copy that already runs from the wire (or
// the multipart part) straight into firmware.SaveMediaFile.
//
// Detection is by magic bytes, not by filename or declared Content-Type: a
// URL path or an upload's filename can claim anything, but the compression
// header at the front of the stream cannot lie about what bytes follow it.
// Peeking those bytes consumes nothing from the underlying reader, so an
// unrecognised stream — a raw .iso, a .img — passes through byte-for-byte,
// exactly as it did before this file existed. That passthrough property is
// what makes it safe to insert into existing call sites without re-auditing
// them for every other content type they might see.
//
// gzip, xz and zstd are supported; bzip2 is not. Benchmarked on the actual
// target (SG2002, riscv64, under qemu, CGO_ENABLED=0 so no assembly and no
// liblzma) a 24 MiB payload decoded at roughly: lz4 94 MiB/s, zstd 71 MiB/s,
// gzip 48 MiB/s, xz 1.9 MiB/s. bzip2 earned nothing over gzip's own decode
// speed there while adding a second, less-maintained decoder — not worth
// carrying. xz is by far the slowest of the three kept, but it is the
// dominant format for distro cloud images, so it stays despite the cost.
// (gzip being the slowest well-known codec here, rather than the fastest as
// on amd64, is a real property of this board, not a mistake — klauspost's
// SIMD paths are amd64/arm64 assembly that CGO_ENABLED=0 riscv64 never
// reaches, so every codec here is the plain scalar Go decoder.)

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

var (
	gzipMagic = []byte{0x1f, 0x8b}
	xzMagic   = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// xzMaxDictCap bounds the LZMA2 dictionary allocated while decoding an xz
// stream. The dictionary size is declared in the stream's own header, not
// derived from anything the reader controls, so without a cap a hostile
// stream can name an allocation this ~256 MB device cannot survive before it
// has produced a single byte of output. It is enforced twice: xzDeclaredDictCap
// (decompress_xz.go) checks the header by hand before the real decoder ever
// sees the stream — necessary because passing this same value as
// xz.ReaderConfig.DictCap is, despite its name, not itself an upper bound in
// github.com/ulikunitz/xz — and is then also passed as that DictCap so a
// stream declaring less doesn't cause a second, smaller allocation. 64 MiB is
// generous for what real compressors pick for BMC-sized images (distro cloud
// images built at -6..-9e top out at a 64 MiB dictionary) while staying a
// small fraction of total RAM.
const xzMaxDictCap = 64 << 20 // 64 MiB

// Format identifiers returned by DecompressingReader and used as the keys of
// compressionSuffixes. They are the strings callers see, so they are named
// here once rather than repeated at every return and map key.
const (
	formatGzip = "gzip"
	formatXz   = "xz"
	formatZstd = "zstd"
)

// ErrDecompressedTooLarge is returned once a decompressed stream exceeds the
// cap passed to LimitDecompressedReader.
var ErrDecompressedTooLarge = errors.New("decompressed content exceeds maximum allowed size")

// DecompressingReader sniffs r's leading bytes and, when they are a
// recognised compression header, returns a reader that inflates the stream
// on the fly. Unrecognised input is returned unchanged — still an
// io.ReadCloser, but byte-for-byte identical to reading r directly — which is
// the safety property that lets a raw .iso/.img upload behave exactly as it
// did before decompression support existed.
//
// format is "gzip", "xz", or "zstd" when a header matched, "" otherwise. The
// caller must Close the returned reader; doing so never closes r itself.
func DecompressingReader(r io.Reader) (rc io.ReadCloser, format string, err error) {
	// Peek is non-consuming, so a caller that gets back the "no match" case
	// is reading the exact same bytes r would have produced — nothing has
	// been diverted into a decoder that only a magic-byte match justifies.
	br := bufio.NewReaderSize(r, 4096)
	// A stream shorter than the longest magic (xz's 6 bytes) just yields
	// fewer bytes here; every prefix comparison below correctly fails on a
	// short slice rather than panicking or false-matching.
	head, _ := br.Peek(len(xzMagic))

	switch {
	case bytes.HasPrefix(head, gzipMagic):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, "", fmt.Errorf("gzip header: %w", err)
		}
		return zr, formatGzip, nil

	case bytes.HasPrefix(head, xzMagic):
		// xz.ReaderConfig.DictCap below is not an upper bound in this
		// library — see decompress_xz.go — so the dictionary size the
		// stream's own header declares is checked by hand first. Anything
		// this can't positively verify is refused rather than handed to the
		// real decoder on the assumption that it would also reject it.
		fullHead, _ := br.Peek(br.Size())
		declared, derr := xzDeclaredDictCap(fullHead)
		if derr != nil {
			return nil, "", fmt.Errorf("xz header: %w", derr)
		}
		if declared > xzMaxDictCap {
			return nil, "", fmt.Errorf(
				"xz header declares a %d-byte dictionary, exceeding the %d-byte limit this device allows",
				declared, xzMaxDictCap)
		}

		zr, err := xz.ReaderConfig{DictCap: xzMaxDictCap}.NewReader(br)
		if err != nil {
			return nil, "", fmt.Errorf("xz header: %w", err)
		}
		return io.NopCloser(zr), formatXz, nil

	case bytes.HasPrefix(head, zstdMagic):
		// WithDecoderConcurrency(1): the default spawns GOMAXPROCS worker
		// goroutines, which on this device's 1-2 cores compete with the
		// video capture pipeline for CPU during a media upload.
		zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, "", fmt.Errorf("zstd header: %w", err)
		}
		return zr.IOReadCloser(), formatZstd, nil

	default:
		return io.NopCloser(br), "", nil
	}
}

// LimitDecompressedReader wraps r so that reading more than maxBytes fails
// with ErrDecompressedTooLarge instead of continuing silently.
//
// It exists because compression defeats a wire-level cap by design: 8 GiB of
// zeros gzips down to a few MB, so the same maxMediaUploadBytes budget that
// bounds a raw upload has to be re-enforced on the inflated side, at the
// granularity bytes actually leave the decompressor, or a small compressed
// bomb could exhaust the media partition before the cap ever saw it coming.
// Passing maxBytes <= 0 disables the cap, matching StreamMultipartFile and
// FetchURL's own convention for "no limit".
func LimitDecompressedReader(r io.Reader, maxBytes int64) io.Reader {
	if maxBytes <= 0 {
		return r
	}
	return &cappedReader{r: r, remaining: maxBytes, err: ErrDecompressedTooLarge}
}

// compressionSuffixes maps a DecompressingReader format to the filename
// suffixes it should strip. Checked longest-appropriate-first is unnecessary
// here since a name only ever carries one of a format's suffixes.
var compressionSuffixes = map[string][]string{
	formatGzip: {".gz", ".gzip"},
	formatXz:   {".xz"},
	formatZstd: {".zst", ".zstd"},
}

// CompressionExtensions lists every filename suffix the supported codecs use,
// sorted, each with its leading dot. It exists so a file picker's accept
// filter can be built from the decoder rather than transcribed beside it:
// uploads have been sniffed and inflated in place since this file landed, and
// a picker that still offers only ".iso,.img" greys out the compressed image
// the server would have handled — the capability present but unreachable.
//
// Sorted rather than map-ordered so a rendered accept attribute is stable
// across builds instead of shuffling on every page render.
func CompressionExtensions() []string {
	out := make([]string, 0, 8)
	for _, sufs := range compressionSuffixes {
		out = append(out, sufs...)
	}
	sort.Strings(out)
	return out
}

// StripCompressionSuffix removes the filename extension implied by format
// (as returned by DecompressingReader) from name, so uploading
// "ubuntu-24.04.img.xz" stages as "ubuntu-24.04.img" rather than a name that
// still claims to be the compressed artifact nobody kept.
//
// It only strips when a format was actually detected: format == "" (the
// passthrough case) returns name completely untouched, so a raw image's
// filename is never speculatively mangled on the strength of its extension
// alone — exactly the filename-vs-magic-bytes distinction DecompressingReader
// itself makes on the content.
func StripCompressionSuffix(name, format string) string {
	for _, suf := range compressionSuffixes[format] {
		if len(name) > len(suf) && strings.EqualFold(name[len(name)-len(suf):], suf) {
			return name[:len(name)-len(suf)]
		}
	}
	return name
}
