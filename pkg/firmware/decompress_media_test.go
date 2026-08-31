package firmware

// decompress_media_test.go pins the interaction between
// utils.DecompressingReader/LimitDecompressedReader and SaveMediaFile itself
// (deliberately left unmodified — see pkg/utils/decompress.go): SaveMediaFile
// already stages to a sibling ".tmp" file and only renames it into place on a
// fully successful copy, so a decompression error or a tripped output cap
// must discard the partial write the same way a plain I/O error already
// does. This test exists to confirm that property still holds once a
// decompressor sits in front of the copy, not to re-test SaveMediaFile's own
// staging logic (see virtual_media_test.go for that).

import (
	"bytes"
	"compress/gzip"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

func gzipOf(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestSaveMediaFileLeavesNoStagedFileOnCorruptStream(t *testing.T) {
	c, _, dir := mediaController(t)

	full := gzipOf(t, bytes.Repeat([]byte("this would have been an image "), 4096))
	truncated := full[:len(full)/2] // cut mid-body: header parses, body doesn't finish

	dr, format, err := utils.DecompressingReader(bytes.NewReader(truncated))
	if err != nil {
		t.Fatalf("DecompressingReader: %v", err)
	}
	defer dr.Close()
	if format != "gzip" {
		t.Fatalf("format = %q, want gzip", format)
	}

	if _, err := c.SaveMediaFile("broken.img", dr); err == nil {
		t.Fatal("SaveMediaFile succeeded on a truncated stream; want an error")
	}

	if exists(t, filepath.Join(dir, "broken.img")) {
		t.Error("a failed decompression must not leave the destination file")
	}
	if exists(t, filepath.Join(dir, "broken.img.tmp")) {
		t.Error("a failed decompression must not leave the sibling temp file either")
	}
}

func TestSaveMediaFileRejectsDecompressionBombAndCleansUp(t *testing.T) {
	c, _, dir := mediaController(t)

	const capBytes = 64 * 1024 // 64 KiB — far below the 16 MiB this bomb inflates to
	bomb := gzipOf(t, make([]byte, 16*1024*1024))

	dr, format, err := utils.DecompressingReader(bytes.NewReader(bomb))
	if err != nil {
		t.Fatalf("DecompressingReader: %v", err)
	}
	defer dr.Close()
	if format != "gzip" {
		t.Fatalf("format = %q, want gzip", format)
	}

	_, err = c.SaveMediaFile("bomb.img", utils.LimitDecompressedReader(dr, capBytes))
	if !errors.Is(err, utils.ErrDecompressedTooLarge) {
		t.Fatalf("err = %v, want ErrDecompressedTooLarge", err)
	}

	if exists(t, filepath.Join(dir, "bomb.img")) {
		t.Error("a rejected decompression bomb must not leave the destination file")
	}
	if exists(t, filepath.Join(dir, "bomb.img.tmp")) {
		t.Error("a rejected decompression bomb must not leave the sibling temp file either")
	}
}
