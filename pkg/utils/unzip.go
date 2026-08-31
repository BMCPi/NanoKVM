package utils

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Unzip extracts filename into dest. Extraction stops with
// ErrDecompressedTooLarge once the archive's entries have written
// maxExtractedBytes in total; see that constant for why the cap sits where it
// does and why its value is what it is.
func Unzip(filename string, dest string) error {
	return unzipCapped(filename, dest, maxExtractedBytes)
}

// unzipCapped is Unzip with the extraction budget as a parameter, so the cap
// can be exercised without producing a quarter-gigabyte of test fixture.
func unzipCapped(filename string, dest string, maxBytes int64) error {
	r, err := zip.OpenReader(filename)
	if err != nil {
		return err
	}
	defer func() {
		_ = r.Close()
	}()

	// Budget shared by every entry, so a bomb cannot slip through as many
	// individually-plausible files.
	remaining := maxBytes

	for _, f := range r.File {
		dstPath := filepath.Join(dest, filepath.Clean("/"+f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return err
			}
			continue
		}

		written, err := unzipFile(dstPath, f, remaining)
		if err != nil {
			return err
		}
		remaining -= written
	}
	return nil
}

// unzipFile extracts f to dstPath, writing at most maxBytes, and reports how
// many bytes it wrote so the caller can carry the budget to the next entry.
func unzipFile(dstPath string, f *zip.File, maxBytes int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = out.Close()
	}()

	archivedFile, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = archivedFile.Close()
	}()

	// io.CopyN, not io.Copy: the +1 lets a copy that fills the whole budget be
	// told apart from one that would have run past it. f.UncompressedSize64 is
	// the archive's own claim about the entry and a hostile archive lies about
	// it, so the budget is spent on bytes that actually left the decompressor.
	written, err := io.CopyN(out, archivedFile, maxBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return written, err
	}
	if written > maxBytes {
		return written, fmt.Errorf("%s: %w", f.Name, ErrDecompressedTooLarge)
	}

	return written, os.Chmod(dstPath, f.Mode())
}
