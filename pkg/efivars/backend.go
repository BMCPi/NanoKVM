package efivars

// backend.go abstracts where the variable store blob physically lives.

import (
	"fmt"
	"os"
)

// Backend reads and writes the raw variable store bytes.
//
// ReadAt/WriteAt use absolute offsets into the store. Size returns the
// store capacity in bytes (0 if unknown).
type Backend interface {
	ReadAt(off int, p []byte) error
	WriteAt(off int, p []byte) error
	Size() int
}

// FileBackend accesses the store through a file: the backing file of a
// kernel i2c-slave-eeprom device (the BMC emulating the EEPROM), an at24
// sysfs "eeprom" node, or a plain file for testing.
type FileBackend struct {
	path string
	size int
}

// NewFileBackend returns a file-backed store. size caps the store capacity;
// pass 0 for unbounded (plain files).
func NewFileBackend(path string, size int) *FileBackend {
	return &FileBackend{path: path, size: size}
}

func (f *FileBackend) Size() int { return f.size }

func (f *FileBackend) ReadAt(off int, p []byte) error {
	fd, err := os.Open(f.path)
	if err != nil {
		return fmt.Errorf("efivars: open store: %w", err)
	}
	defer fd.Close()
	if _, err := fd.ReadAt(p, int64(off)); err != nil {
		return fmt.Errorf("efivars: read store: %w", err)
	}
	return nil
}

func (f *FileBackend) WriteAt(off int, p []byte) error {
	if f.size > 0 && off+len(p) > f.size {
		return fmt.Errorf("efivars: blob (%d bytes) exceeds store size (%d)", off+len(p), f.size)
	}
	fd, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("efivars: open store: %w", err)
	}
	defer fd.Close()
	if _, err := fd.WriteAt(p, int64(off)); err != nil {
		return fmt.Errorf("efivars: write store: %w", err)
	}
	return fd.Sync()
}
