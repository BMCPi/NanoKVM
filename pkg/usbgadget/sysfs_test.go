package usbgadget

import (
	"os"
	"path/filepath"
	"testing"
)

// A path resolving outside the root — even one built by joining onto the root —
// must be refused before any filesystem call happens.
func TestSysfsRejectsPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	s := newSysfs(root)

	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReadFile(outside); err == nil {
		t.Fatalf("ReadFile(%q) succeeded; a path outside the sysfs root must be rejected", outside)
	}
	if err := s.WriteFile(outside, []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile outside the sysfs root must be rejected")
	}
}

// The security property this whole service exists for: the gadget tree is built
// from symlinks, and a symlink that points out of the root must not let a read
// or write follow it out. os.Root enforces this at the operation.
func TestSysfsBlocksSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "passwd"), []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the root pointing outside it — the exact shape of the
	// gadget's own configs/c.1 function symlinks.
	if err := os.Symlink(secretDir, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	s := newSysfs(root)
	if _, err := s.ReadFile(filepath.Join(root, "escape", "passwd")); err == nil {
		t.Fatal("ReadFile through an escaping symlink succeeded; os.Root confinement failed")
	}
}

// writeAttrIfDifferent must treat a trailing-newline-only difference as equal
// (configfs read-back adds one) and still write a genuinely different value.
func TestSysfsWriteAttrIfDifferent(t *testing.T) {
	root := t.TempDir()
	s := newSysfs(root)
	attr := filepath.Join(root, "idVendor")

	if err := s.writeAttr(attr, "0x3346"); err != nil {
		t.Fatalf("writeAttr: %v", err)
	}
	if err := s.writeAttrIfDifferent(attr, "0x3346\n"); err != nil {
		t.Fatalf("writeAttrIfDifferent (unchanged): %v", err)
	}
	if err := s.writeAttrIfDifferent(attr, "0x1d6b"); err != nil {
		t.Fatalf("writeAttrIfDifferent (changed): %v", err)
	}
	got, err := s.ReadFile(attr)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "0x1d6b" {
		t.Fatalf("attr = %q, want %q", got, "0x1d6b")
	}
}

// ReadDir must return entries sorted by name, matching os.ReadDir, because the
// configfs symlink-ordering logic depends on a stable order.
func TestSysfsReadDirSorted(t *testing.T) {
	root := t.TempDir()
	s := newSysfs(root)
	for _, n := range []string{"b", "a", "c"} {
		if err := s.WriteFile(filepath.Join(root, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := make([]string, len(entries))
	for i, e := range entries {
		got[i] = e.Name()
	}
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReadDir names = %v, want %v", got, want)
		}
	}
}
