package rpieeprom

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// buildSyntheticImage constructs a 512 KB image with bootcode + bootconf.txt
// where bootconf is the LAST file section. This matches the usual real-world
// layout (bootconf trailing the partition with the reserved scratch after)
// and lets update-grow tests use the whole partition as the available slot.
func buildSyntheticImage(t *testing.T, bootcodeContent []byte, bootconfContent string) []byte {
	t.Helper()
	const imageSize = 512 * 1024
	data := newBlankImage(imageSize)
	writeBootcode(data, bootcodeContent)
	writeBootconf(data, ReadOnlySize, bootconfContent)
	return data
}

// buildSyntheticImageWithTrailing is like buildSyntheticImage but appends a
// dummy pubkey.bin section right after bootconf. Use this when a test needs
// bootconf NOT to be the last section — e.g. to exercise the PadMagic
// insertion path on shrink updates.
func buildSyntheticImageWithTrailing(t *testing.T, bootcodeContent []byte, bootconfContent string) []byte {
	t.Helper()
	const imageSize = 512 * 1024
	data := newBlankImage(imageSize)
	writeBootcode(data, bootcodeContent)
	bootconfEnd := writeBootconf(data, ReadOnlySize, bootconfContent)
	writeFileSection(data, bootconfEnd, PubKeyBin, bytes.Repeat([]byte{0xAA}, 16))
	return data
}

func newBlankImage(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = 0xff
	}
	return data
}

// writeBootcode writes Section 0 (Magic) at offset 0. The declared length
// is ReadOnlySize-8 so the parser's `offset += 8 + length` lands exactly on
// ReadOnlySize, walking past the 0xff fill (which would otherwise look like
// an EOF magic) without needing a separate PadMagic section.
func writeBootcode(data, content []byte) {
	binary.BigEndian.PutUint32(data[0:4], Magic)
	binary.BigEndian.PutUint32(data[4:8], uint32(ReadOnlySize-8))
	copy(data[8:8+len(content)], content)
}

// writeBootconf writes a FileMagic bootconf.txt section at off with its
// declared length matching the content (real EEPROM behaviour — the
// section "slot" is implicit, defined by the next section's offset).
// Returns the next-8-aligned offset where a following section could go.
func writeBootconf(data []byte, off int, content string) int {
	return writeFileSection(data, off, BootConfTxt, []byte(content))
}

// writeFileSection writes a FileMagic section at off and returns the next
// 8-aligned offset suitable for placing a subsequent section.
func writeFileSection(data []byte, off int, name string, content []byte) int {
	length := FilenameLen + 4 + len(content)
	binary.BigEndian.PutUint32(data[off:off+4], FileMagic)
	binary.BigEndian.PutUint32(data[off+4:off+8], uint32(length))
	copy(data[off+8:off+8+FilenameLen], []byte(name))
	copy(data[off+4+FileHdrLen:off+4+FileHdrLen+len(content)], content)
	next := off + 8 + length
	return (next + 7) &^ 7
}

func TestParse_RejectsInvalidSize(t *testing.T) {
	if _, err := ParseBytes(make([]byte, 1024)); err == nil {
		t.Fatal("expected error for non-standard image size")
	}
}

func TestParse_FindsSections(t *testing.T) {
	img, err := ParseBytes(buildSyntheticImage(t, []byte("hello-bootcode"), "[all]\nBOOT_UART=1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	secs := img.Sections()
	if len(secs) < 2 {
		t.Fatalf("expected at least 2 sections, got %d", len(secs))
	}
	if secs[0].Magic != Magic || secs[0].Offset != 0 {
		t.Errorf("section[0] = %+v; expected bootcode at offset 0", secs[0])
	}
	var bootconf *Section
	for i := range secs {
		if secs[i].Filename == BootConfTxt {
			bootconf = &secs[i]
		}
	}
	if bootconf == nil {
		t.Fatal("bootconf.txt section not found")
	}
	if bootconf.Magic != FileMagic {
		t.Errorf("bootconf magic = %#x; want FileMagic", bootconf.Magic)
	}
	if bootconf.Offset != ReadOnlySize {
		t.Errorf("bootconf offset = %d; want %d", bootconf.Offset, ReadOnlySize)
	}
}

func TestGetFile_BootConf(t *testing.T) {
	want := "[all]\nBOOT_UART=1\nBOOT_ORDER=0xf41\n"
	img, err := ParseBytes(buildSyntheticImage(t, []byte("x"), want))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := img.GetFile(BootConfTxt)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if string(got) != want {
		t.Errorf("GetFile(bootconf.txt) = %q; want %q", got, want)
	}
}

func TestGetFile_Bootcode(t *testing.T) {
	// GetFile returns the full declared bootcode section (ReadOnlySize-8 in
	// our synthetic image, since real EEPROMs reserve the entire read-only
	// header for bootcode). Just verify the prefix bytes match.
	bootcode := []byte("ABCDEFGH")
	img, err := ParseBytes(buildSyntheticImage(t, bootcode, "[all]\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := img.GetFile(BootCodeBin)
	if err != nil {
		t.Fatalf("GetFile bootcode: %v", err)
	}
	if len(got) != ReadOnlySize-8 {
		t.Errorf("GetFile(bootcode) length = %d; want %d", len(got), ReadOnlySize-8)
	}
	if !bytes.Equal(got[:len(bootcode)], bootcode) {
		t.Errorf("GetFile(bootcode) prefix = %q; want %q", got[:len(bootcode)], bootcode)
	}
}

func TestUpdateFile_RoundTrip(t *testing.T) {
	initial := "[all]\nBOOT_UART=1\n"
	img, err := ParseBytes(buildSyntheticImage(t, []byte("bc"), initial))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Replace with a larger config to also exercise that the length field
	// is written correctly.
	want := "[all]\nBOOT_UART=0\nBOOT_ORDER=0xf41\nWAKE_ON_GPIO=1\nPSU_MAX_CURRENT=5000\n"
	if err := img.UpdateFile(BootConfTxt, []byte(want)); err != nil {
		t.Fatalf("UpdateFile: %v", err)
	}

	// Reparse from the mutated bytes to confirm the section table still
	// walks cleanly (no magic-mask violations from leftover bytes).
	roundTripped, err := ParseBytes(img.Bytes())
	if err != nil {
		t.Fatalf("re-parse after update: %v", err)
	}
	got, err := roundTripped.GetFile(BootConfTxt)
	if err != nil {
		t.Fatalf("GetFile after update: %v", err)
	}
	if string(got) != want {
		t.Errorf("round-trip mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestUpdateFile_Shrink_InsertsPadding(t *testing.T) {
	// Long → short forces padTo to write a PAD_MAGIC header so the parser
	// can walk past the freed bytes. Use the trailing-section image so
	// bootconf isn't the last section (padding skipped in that case).
	initial := strings.Repeat("X", 1024)
	img, err := ParseBytes(buildSyntheticImageWithTrailing(t, []byte("bc"), initial))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := img.UpdateFile(BootConfTxt, []byte("[all]\n")); err != nil {
		t.Fatalf("UpdateFile: %v", err)
	}
	// Re-parse and confirm everything still walks.
	if _, err := ParseBytes(img.Bytes()); err != nil {
		t.Fatalf("re-parse after shrink: %v", err)
	}
	// Find a PadMagic section.
	reparsed, _ := ParseBytes(img.Bytes())
	foundPad := false
	for _, s := range reparsed.Sections() {
		if s.Magic == PadMagic {
			foundPad = true
			break
		}
	}
	if !foundPad {
		t.Error("expected a PadMagic section after shrinking bootconf.txt; found none")
	}
}

func TestUpdateFile_RejectsTooLarge(t *testing.T) {
	img, err := ParseBytes(buildSyntheticImage(t, []byte("bc"), "[all]\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	huge := make([]byte, MaxFileSize+1)
	if err := img.UpdateFile(BootConfTxt, huge); err == nil {
		t.Error("expected error for content larger than MaxFileSize")
	}
}

func TestUpdateFile_RejectsUnknown(t *testing.T) {
	img, err := ParseBytes(buildSyntheticImage(t, []byte("bc"), "[all]\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := img.UpdateFile("nonexistent.txt", []byte("hi")); err == nil {
		t.Error("expected error for unknown filename")
	}
}
