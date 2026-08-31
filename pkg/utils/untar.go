package utils

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxExtractedBytes bounds the total number of bytes one archive extraction
// (UnTarGz, Unzip) may write to disk.
//
// A cap is needed because compression hides an archive's real cost: a few MB
// of compressed zeros inflate to gigabytes, so the size of the file that
// arrived says nothing about the space unpacking it will consume. The only
// place that cost is observable is on the way out of the decompressor, which
// is why it is counted there rather than checked up front.
//
// The bound has to clear every legitimate update package by a wide margin.
// Measured: the current release archive (nanokvm-app_2.3.15.tar.gz) unpacks
// to 11.1 MB total, 10.5 MB of that the UPX-packed server binary; the largest
// package this project has carried is the ~53 MB tarball that CacheDir's
// comment in pkg/application/dirs.go still refers to. 256 MiB is roughly five
// times that historical high-water mark and over twenty times what ships
// today, so no plausible future package is at risk — while still refusing the
// multi-gigabyte expansion a decompression bomb needs to be worth mounting
// against /var/lib/nanokvm/cache.
const maxExtractedBytes = 256 << 20 // 256 MiB

// UnTarGz extracts srcFile (a .tar.gz) into destDir. Parent directories are
// created on demand for entries whose tar stream does not include explicit
// directory headers. Returns destDir on success.
//
// Extraction stops with ErrDecompressedTooLarge once the archive's entries
// have written maxExtractedBytes in total.
func UnTarGz(srcFile string, destDir string) (string, error) {
	return unTarGzCapped(srcFile, destDir, maxExtractedBytes)
}

// unTarGzCapped is UnTarGz with the extraction budget as a parameter, so the
// cap can be exercised without producing a quarter-gigabyte of test fixture.
func unTarGzCapped(srcFile string, destDir string, maxBytes int64) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}

	fr, err := os.Open(srcFile)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = fr.Close()
	}()

	gr, err := gzip.NewReader(fr)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = gr.Close()
	}()

	tr := tar.NewReader(gr)

	// Budget shared by every entry, so a bomb cannot slip through as many
	// individually-plausible files.
	remaining := maxBytes

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		// Reject path traversal (e.g. "../etc/passwd").
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, ".."+string(os.PathSeparator)) || cleanName == ".." {
			continue
		}

		filename := filepath.Join(absDest, cleanName)
		if !strings.HasPrefix(filename, absDest+string(os.PathSeparator)) && filename != absDest {
			continue
		}

		// header.Mode is an int64 carrying Unix permission bits. The nine
		// masked off here are the only ones os.FileMode defines in the same
		// positions, and the only ones os.OpenFile/os.MkdirAll would forward
		// to the kernel anyway (they call FileMode.Perm() internally), so the
		// mask is what the syscall layer already does, made explicit — and it
		// is what makes the int64 -> uint32 conversion provably in range
		// (G115), rather than trusting an archive's declared mode.
		mode := os.FileMode(header.Mode & 0o777)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filename, mode); err != nil {
				return "", err
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
				return "", err
			}
			file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
			if err != nil {
				return "", err
			}
			// io.CopyN, not io.Copy: the +1 lets a copy that fills the whole
			// remaining budget be told apart from one that would have run
			// past it. header.Size is the archive's own claim about the entry
			// and a hostile archive lies about it, so the budget is spent on
			// bytes that actually left the decompressor.
			written, err := io.CopyN(file, tr, remaining+1)
			_ = file.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				return "", err
			}
			if written > remaining {
				return "", fmt.Errorf("%s: %w", header.Name, ErrDecompressedTooLarge)
			}
			remaining -= written

		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
				return "", err
			}
			_ = os.Remove(filename)
			if err := os.Symlink(header.Linkname, filename); err != nil {
				return "", err
			}
		}
	}

	return absDest, nil
}
