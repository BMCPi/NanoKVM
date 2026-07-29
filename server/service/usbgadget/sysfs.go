package usbgadget

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// sysfs is the service that owns every filesystem operation the gadget performs
// under /sys: the configfs tree at /sys/kernel/config/usb_gadget, the UDC list
// at /sys/class/udc, and the dwc2 driver bind/unbind files. All of them go
// through a single os.Root scoped to sysfsRootPath, so a symlink inside configfs
// (the gadget is largely a tree of symlinks) — or a bug that builds a stray
// path — can never redirect a read or write to a file outside /sys.
//
// Only /sys paths belong here. The OTG role switch (/proc/cviusb/otg_role),
// /proc/mounts, the persisted state file and boot flags on /data and /boot, and
// the ISO images the LUNs point at are all outside the root and stay on plain
// os.* in their respective files.
//
// It is a typed service rather than a set of package-level helpers so the root
// is owned, lazily opened once, and mockable in tests via sysfsRootPath — the
// same override discipline the other path roots (gadgetRoot, configFSPath,
// bootDir) already use.
type sysfs struct {
	rootPath string

	mu   sync.Mutex
	root *os.Root
}

func newSysfs(rootPath string) *sysfs {
	return &sysfs{rootPath: rootPath}
}

// open lazily opens and caches the os.Root. Deferring the open keeps Gadget
// construction infallible and means a build that never touches the tree (the
// gadget disabled in config) never requires sysfsRootPath to exist.
func (s *sysfs) open() (*os.Root, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		root, err := os.OpenRoot(s.rootPath)
		if err != nil {
			return nil, fmt.Errorf("open sysfs root %s: %w", s.rootPath, err)
		}
		s.root = root
	}
	return s.root, nil
}

// resolve translates an absolute path under the root into (root, root-relative
// path), rejecting anything that would escape the root.
func (s *sysfs) resolve(path string) (*os.Root, string, error) {
	root, err := s.open()
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(s.rootPath, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, "", fmt.Errorf("path %s is outside sysfs root %s", path, s.rootPath)
	}
	return root, rel, nil
}

// ---- low-level os.Root-scoped primitives -----------------------------------

func (s *sysfs) ReadFile(path string) ([]byte, error) {
	root, rel, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return root.ReadFile(rel)
}

func (s *sysfs) WriteFile(path string, data []byte, perm os.FileMode) error {
	root, rel, err := s.resolve(path)
	if err != nil {
		return err
	}
	return root.WriteFile(rel, data, perm)
}

func (s *sysfs) Stat(path string) (os.FileInfo, error) {
	root, rel, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return root.Stat(rel)
}

func (s *sysfs) Lstat(path string) (os.FileInfo, error) {
	root, rel, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return root.Lstat(rel)
}

// ReadDir returns the directory entries sorted by filename, matching os.ReadDir.
func (s *sysfs) ReadDir(path string) ([]os.DirEntry, error) {
	root, rel, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

func (s *sysfs) Mkdir(path string, perm os.FileMode) error {
	root, rel, err := s.resolve(path)
	if err != nil {
		return err
	}
	return root.Mkdir(rel, perm)
}

func (s *sysfs) MkdirAll(path string, perm os.FileMode) error {
	root, rel, err := s.resolve(path)
	if err != nil {
		return err
	}
	return root.MkdirAll(rel, perm)
}

// Symlink creates a symlink at path pointing to target. target is stored
// verbatim (it may be the absolute configfs function path the kernel expects);
// only the link location is confined to the root.
func (s *sysfs) Symlink(target, path string) error {
	root, rel, err := s.resolve(path)
	if err != nil {
		return err
	}
	return root.Symlink(target, rel)
}

func (s *sysfs) Remove(path string) error {
	root, rel, err := s.resolve(path)
	if err != nil {
		return err
	}
	return root.Remove(rel)
}

// ---- configfs attribute conveniences ---------------------------------------

// writeAttr writes value to a configfs attribute file verbatim. configfs is
// lenient about trailing newlines.
func (s *sysfs) writeAttr(path, value string) error {
	return s.WriteFile(path, []byte(value), 0o644)
}

// writeAttrIfDifferent writes value only when the file's current (trimmed)
// contents differ. This avoids EBUSY when re-asserting unchanged descriptor
// fields on an already-bound gadget — the common server-restart case where the
// gadget already exists and must not be disturbed.
func (s *sysfs) writeAttrIfDifferent(path, value string) error {
	cur, err := s.ReadFile(path)
	if err == nil && strings.TrimSpace(string(cur)) == strings.TrimSpace(value) {
		return nil
	}
	return s.writeAttr(path, value)
}

// writeReportDesc writes an HID report descriptor, skipping the write when the
// existing contents already match byte-for-byte.
func (s *sysfs) writeReportDesc(path string, desc []byte) error {
	if cur, err := s.ReadFile(path); err == nil && bytes.Equal(cur, desc) {
		return nil
	}
	return s.WriteFile(path, desc, 0o644)
}
