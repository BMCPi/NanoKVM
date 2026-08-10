package firmware

// files.go provides per-file read/write/remove operations against the
// firmware image, plus a bulk SyncFirmwareDirToImage that copies every
// file under c.firmwareDir into the (mounted) image, replacing existing
// files at the same relative paths.
//
// All operations use withMount() so the gadget LUN file is emptied for
// the duration and restored afterwards.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

// imagePathFor maps a user-supplied name (relative or absolute) to its
// absolute path under c.mountPoint, rejecting traversal.
func (c *Controller) imagePathFor(name string) (string, error) {
	if c.mountPoint == "" {
		return "", fmt.Errorf("mountPoint not configured")
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(name, "/"))
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("invalid name %q", name)
	}
	return filepath.Join(c.mountPoint, clean), nil
}

// ReadFileFromImage reads a named file from the mounted image.
// Returns (nil, nil) if the file does not exist.
func (c *Controller) ReadFileFromImage(name string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var data []byte
	err := c.withMount(func() error {
		path, err := c.imagePathFor(name)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		data = b
		return nil
	})
	return data, err
}

// WriteReaderToImage streams r into the named file inside the FAT image without
// buffering the whole payload in memory (unlike WriteFileToImage). Returns the
// number of bytes written.
func (c *Controller) WriteReaderToImage(name string, r io.Reader) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.invalidateReaderCacheLocked()

	var written int64
	err := c.withMount(func() error {
		path, err := c.imagePathFor(name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mkdir parent: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		written, err = io.Copy(f, r)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", path, closeErr)
		}
		log.Infof("firmware: wrote %d bytes → %s", written, path)
		return nil
	})
	return written, err
}

// WriteFileToImage writes data to a named file in the mounted image.
func (c *Controller) WriteFileToImage(name string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.invalidateReaderCacheLocked()

	return c.withMount(func() error {
		path, err := c.imagePathFor(name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mkdir parent: %w", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		log.Infof("firmware: wrote %d bytes → %s", len(data), path)
		return nil
	})
}

// RemoveFileFromImage deletes a file from the mounted image.
func (c *Controller) RemoveFileFromImage(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.invalidateReaderCacheLocked()

	return c.withMount(func() error {
		path, err := c.imagePathFor(name)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		log.Infof("firmware: removed %s", path)
		return nil
	})
}

// ListFilesInImage returns names of all entries in the image FAT root
// (root level only).
func (c *Controller) ListFilesInImage() ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var names []string
	err := c.withMount(func() error {
		entries, err := os.ReadDir(c.mountPoint)
		if err != nil {
			return err
		}
		names = make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return nil
	})
	return names, err
}

// SyncFirmwareDirToImage iterates over c.firmwareDir and copies each file
// into the mounted image at the same relative path, replacing existing
// files. Directories are created as needed. Files in the image that have
// no counterpart in c.firmwareDir are left untouched (incl. env files).
func (c *Controller) SyncFirmwareDirToImage() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.invalidateReaderCacheLocked()

	if c.firmwareDir == "" {
		return fmt.Errorf("firmwareDir not configured")
	}
	if _, err := os.Stat(c.firmwareDir); err != nil {
		return fmt.Errorf("firmware dir %s: %w", c.firmwareDir, err)
	}

	return c.withMount(func() error {
		var copied int
		err := filepath.Walk(c.firmwareDir, func(srcPath string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(c.firmwareDir, srcPath)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			destPath := filepath.Join(c.mountPoint, rel)
			if info.IsDir() {
				if err := os.MkdirAll(destPath, 0o755); err != nil {
					return fmt.Errorf("mkdir %s: %w", destPath, err)
				}
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", destPath, err)
			}
			if err := copyHostFile(srcPath, destPath); err != nil {
				return fmt.Errorf("copy %s: %w", rel, err)
			}
			copied++
			return nil
		})
		if err != nil {
			return err
		}
		log.Infof("firmware: synced %d files from %s into image", copied, c.firmwareDir)
		return nil
	})
}

// copyHostFile copies src to dest with file perms 0644.
func copyHostFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
