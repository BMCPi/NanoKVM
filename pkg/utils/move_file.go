package utils

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

func MoveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := os.Rename(src, dst)
	if err != nil {
		if strings.Contains(err.Error(), "invalid cross-device link") {
			return MoveFileCrossFS(src, dst)
		}
		return err
	}
	return nil
}

func MoveFileCrossFS(src, dst string) error {
	tmp := dst + ".tmp"
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}

	tmpFile, err := os.Create(tmp)
	if err != nil {
		_ = srcFile.Close()
		return err
	}
	_, err = io.Copy(tmpFile, srcFile)
	if err != nil {
		_ = srcFile.Close()
		_ = tmpFile.Close()
		return err
	}
	_ = srcFile.Close()
	_ = tmpFile.Close()
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	err = os.Chmod(tmp, fi.Mode())
	if err != nil {
		return err
	}
	_ = os.Remove(src)
	err = os.Rename(tmp, dst)
	if err != nil {
		return err
	}
	return nil
}

func MoveFilesRecursively(src, dst string) error {
	return filepath.Walk(src, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		fileName := strings.Replace(path, src, "", 1)
		dstName := dst + fileName
		fileInfo, err := os.Stat(path)
		if err != nil {
			return err
		}

		if fileInfo.IsDir() {
			// G122 flags this for sitting in a Walk callback, but the path it
			// creates is not the walked one: dstName is the caller's dst with
			// a suffix Walk produced from underneath src, so it cannot escape
			// dst, and dst is always a fixed BMC-owned install directory
			// (/var/lib/nanokvm/app or app.prev). The walked tree itself is
			// written by this process alone — see ChmodRecursively for the
			// same trust argument in full.
			//nolint:gosec // G122: dstName is rooted at the caller's dst, not at the walked path
			return os.MkdirAll(dstName, fileInfo.Mode())
		}
		return MoveFile(path, dstName)
	})
}
