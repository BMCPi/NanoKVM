package utils

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UnTarGz extracts srcFile (a .tar.gz) into destDir. Parent directories are
// created on demand for entries whose tar stream does not include explicit
// directory headers. Returns destDir on success.
func UnTarGz(srcFile string, destDir string) (string, error) {
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

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filename, os.FileMode(header.Mode)); err != nil {
				return "", err
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
				return "", err
			}
			file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(file, tr); err != nil {
				_ = file.Close()
				return "", err
			}
			_ = file.Close()

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
