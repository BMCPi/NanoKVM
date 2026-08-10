package ubootenv

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

const (
	testChipSize = 0x8000 // 24c256
	testOffset   = 0x4000 // CONFIG_ENV_OFFSET
	testEnvSize  = 0x4000 // CONFIG_ENV_SIZE
)

// memBackend is an in-memory Backend spanning [0, len(buf)).
type memBackend struct{ buf []byte }

func newMemBackend(size int) *memBackend { return &memBackend{buf: make([]byte, size)} }

func (m *memBackend) Size() int { return len(m.buf) }

func (m *memBackend) ReadAt(off int, p []byte) error {
	if off < 0 || off+len(p) > len(m.buf) {
		return fmt.Errorf("read [%d,%d) out of range", off, off+len(p))
	}
	copy(p, m.buf[off:])
	return nil
}

func (m *memBackend) WriteAt(off int, p []byte) error {
	if off < 0 || off+len(p) > len(m.buf) {
		return fmt.Errorf("write [%d,%d) out of range", off, off+len(p))
	}
	copy(m.buf[off:], p)
	return nil
}

func newTestStore(t *testing.T, b Backend) *Store {
	t.Helper()
	return NewStore(b, testOffset, testEnvSize, filepath.Join(t.TempDir(), "env.bin"))
}

// A blank EEPROM must read as an empty environment, not an error — the first
// boot has to work.
func TestStoreLoadBlank(t *testing.T) {
	s := newTestStore(t, newMemBackend(testChipSize))
	env, err := s.Load()
	if err != nil {
		t.Fatalf("Load on blank store: %v", err)
	}
	if len(env.Vars) != 0 {
		t.Fatalf("blank store returned %d vars, want 0", len(env.Vars))
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := newTestStore(t, newMemBackend(testChipSize))

	if err := s.Update(func(e *Env) {
		e.Set(VarSerial, "ds=nocloud-net;s=http://x/configs/;h=node1")
		e.Set(VarBootTargets, "usb0 nvme0")
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	s.Invalidate() // force a re-read of the region rather than the cache
	env, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, _ := env.Get(VarSerial); got != "ds=nocloud-net;s=http://x/configs/;h=node1" {
		t.Errorf("serial# = %q", got)
	}
	if got, _ := env.Get(VarBootTargets); got != "usb0 nvme0" {
		t.Errorf("boot_targets = %q", got)
	}
	if env.Format != FormatBinary {
		t.Errorf("Format = %v, want FormatBinary", env.Format)
	}
}

// The env must land at CONFIG_ENV_OFFSET and never touch the UEFI variable
// blob that lives below it on the same chip.
func TestStoreWritesOnlyItsRegion(t *testing.T) {
	b := newMemBackend(testChipSize)
	// Seed the UEFI region with a sentinel.
	for i := range testOffset {
		b.buf[i] = 0xAA
	}
	s := newTestStore(t, b)

	if err := s.Update(func(e *Env) { e.Set("foo", "bar") }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for i := range testOffset {
		if b.buf[i] != 0xAA {
			t.Fatalf("UEFI region clobbered at %#x: %#x", i, b.buf[i])
		}
	}
	// The env region must now hold a valid, parseable env.
	if _, ok := tryParseBinary(b.buf[testOffset : testOffset+testEnvSize]); !ok {
		t.Fatal("env region does not hold a valid binary env")
	}
}

func TestStoreUpdateDeletes(t *testing.T) {
	s := newTestStore(t, newMemBackend(testChipSize))
	if err := s.Update(func(e *Env) { e.Set("keep", "1"); e.Set("drop", "2") }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.Update(func(e *Env) { e.Delete("drop") }); err != nil {
		t.Fatalf("Update delete: %v", err)
	}
	s.Invalidate()
	env, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := env.Get("drop"); ok {
		t.Error("drop still present after delete")
	}
	if got, _ := env.Get("keep"); got != "1" {
		t.Errorf("keep = %q, want 1", got)
	}
}

// A BMC reboot wipes the volatile i2c-slave-eeprom RAM; reconcile must restore
// the durable snapshot so the host still finds its environment.
func TestStoreReconcileRestoresSnapshot(t *testing.T) {
	snap := filepath.Join(t.TempDir(), "env.bin")

	b := newMemBackend(testChipSize)
	s := NewStore(b, testOffset, testEnvSize, snap)
	if err := s.Update(func(e *Env) { e.Set("serial#", "abc123") }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	saved := append([]byte(nil), b.buf[testOffset:testOffset+testEnvSize]...)

	// Simulate the BMC reboot: the EEPROM comes back blank, snapshot survives.
	blank := newMemBackend(testChipSize)
	s2 := NewStore(blank, testOffset, testEnvSize, snap)
	s2.mu.Lock()
	s2.reconcileLocked()
	s2.mu.Unlock()

	if !bytes.Equal(blank.buf[testOffset:testOffset+testEnvSize], saved) {
		t.Fatal("env region was not restored from the snapshot")
	}
	env, err := s2.Load()
	if err != nil {
		t.Fatalf("Load after restore: %v", err)
	}
	if got, _ := env.Get("serial#"); got != "abc123" {
		t.Errorf("serial# = %q, want abc123", got)
	}
}

// A populated EEPROM is authoritative (U-Boot may have just run saveenv), so
// reconcile must mirror it into the snapshot rather than overwrite it.
func TestStoreReconcilePersistsFromEEPROM(t *testing.T) {
	snap := filepath.Join(t.TempDir(), "env.bin")

	b := newMemBackend(testChipSize)
	seed := NewStore(b, testOffset, testEnvSize, "") // no snapshot: EEPROM only
	if err := seed.Update(func(e *Env) { e.Set("ver", "U-Boot 2026.04") }); err != nil {
		t.Fatalf("seed Update: %v", err)
	}

	s := NewStore(b, testOffset, testEnvSize, snap)
	s.mu.Lock()
	s.reconcileLocked()
	s.mu.Unlock()

	restored := readSnapshot(snap, testEnvSize)
	if restored == nil {
		t.Fatal("snapshot was not written from the EEPROM")
	}
	env, ok := tryParseBinary(restored)
	if !ok {
		t.Fatal("snapshot is not a valid binary env")
	}
	if got, _ := env.Get("ver"); got != "U-Boot 2026.04" {
		t.Errorf("ver = %q", got)
	}
}

func TestStoreUnconfigured(t *testing.T) {
	var s *Store
	if s.Available() {
		t.Error("nil store reports Available")
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Load on nil store = %v, want ErrNotConfigured", err)
	}
	if err := s.Update(func(*Env) {}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Update on nil store = %v, want ErrNotConfigured", err)
	}
}
