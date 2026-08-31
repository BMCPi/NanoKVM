package streamio

// decompress_xz_test.go pins xzDeclaredDictCap against real xz streams: it
// must read exactly the dictionary size xz.WriterConfig encoded, and
// DecompressingReader must refuse a stream that declares more than
// xzMaxDictCap before ever constructing the real decoder — the property that
// makes xzMaxDictCap an actual ceiling rather than the floor
// xz.ReaderConfig.DictCap alone would give it (see decompress_xz.go).

import (
	"bytes"
	"io"
	"testing"

	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

func xzBytesWithDictCap(t *testing.T, payload []byte, dictCap int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := xz.WriterConfig{DictCap: dictCap}.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz writer (DictCap=%d): %v", dictCap, err)
	}
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("xz write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("xz close: %v", err)
	}
	return buf.Bytes()
}

func TestXZDeclaredDictCapMatchesWhatWasEncoded(t *testing.T) {
	payload := []byte("dictionary size probe")

	for _, want := range []int{1 << 16, 1 << 20, 8 << 20, 64 << 20} {
		compressed := xzBytesWithDictCap(t, payload, want)

		got, err := xzDeclaredDictCap(compressed)
		if err != nil {
			t.Fatalf("xzDeclaredDictCap(DictCap=%d): %v", want, err)
		}

		// EncodeDictCap rounds UP to the nearest representable code, so the
		// header's actual declared value can exceed what we asked for; the
		// value we must match is what encoding-then-decoding produces, not
		// the raw request.
		wantDecoded, err := lzma.DecodeDictCap(lzma.EncodeDictCap(int64(want)))
		if err != nil {
			t.Fatalf("lzma.DecodeDictCap/EncodeDictCap(%d): %v", want, err)
		}
		if got != wantDecoded {
			t.Errorf("DictCap=%d: xzDeclaredDictCap = %d, want %d", want, got, wantDecoded)
		}
	}
}

func TestXZDeclaredDictCapRejectsTruncatedHeader(t *testing.T) {
	full := xzBytesWithDictCap(t, []byte("truncated header probe"), 1<<20)

	// Cut inside the block header (right after the stream header, well
	// before the header's own declared length is satisfied).
	truncated := full[:xz.HeaderLen+2]
	if _, err := xzDeclaredDictCap(truncated); err == nil {
		t.Fatal("want an error for a block header cut short, got nil")
	}
}

func TestXZDeclaredDictCapRejectsCorruptChecksum(t *testing.T) {
	full := xzBytesWithDictCap(t, []byte("checksum probe"), 1<<20)
	corrupt := append([]byte(nil), full...)
	// Flip a bit inside the block header flags byte without touching its
	// trailing CRC32, so the checksum this function itself verifies no
	// longer matches.
	corrupt[xz.HeaderLen+1] ^= 0xFF

	if _, err := xzDeclaredDictCap(corrupt); err == nil {
		t.Fatal("want an error for a corrupted block header, got nil")
	}
}

// The actual ceiling this whole file exists for: a stream whose header
// declares a dictionary larger than xzMaxDictCap must be refused by
// DecompressingReader itself, before the real xz.NewReader (and the eager
// allocation inside it) ever runs.
func TestDecompressingReaderRejectsOversizedXZDictionary(t *testing.T) {
	oversized := xzBytesWithDictCap(t, bytes.Repeat([]byte("x"), 4096), xzMaxDictCap*2)

	_, _, err := DecompressingReader(bytes.NewReader(oversized))
	if err == nil {
		t.Fatal("want an error for an xz stream declaring a dictionary above the cap, got nil")
	}
}

// A declared size at or under the cap must still decode normally — the
// pre-check is a ceiling, not an accidental new restriction on ordinary
// files.
func TestDecompressingReaderAcceptsXZDictionaryAtCap(t *testing.T) {
	payload := bytes.Repeat([]byte("well within the cap "), 4096)
	compressed := xzBytesWithDictCap(t, payload, xzMaxDictCap)

	rc, format, err := DecompressingReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("DecompressingReader: %v", err)
	}
	defer rc.Close()
	if format != "xz" {
		t.Fatalf("format = %q, want xz", format)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("round trip mismatch for a dictionary size exactly at the cap")
	}
}

func TestXZDeclaredDictCapRejectsIndexIndicator(t *testing.T) {
	// A block header size byte of 0 is the xz index indicator, not a block
	// at all — valid in the format, but not something this parser (which
	// only ever looks at the first block) should treat as one.
	head := make([]byte, xz.HeaderLen+4)
	copy(head, xzMagic)
	// head[xz.HeaderLen] (the block header size byte) is already 0.
	if _, err := xzDeclaredDictCap(head); err == nil {
		t.Fatal("want an error for a zero block-header-size byte, got nil")
	}
}
