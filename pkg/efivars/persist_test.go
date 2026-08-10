package efivars

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// persistFixture wires a file-backed "EEPROM" and a snapshot path into a
// Manager, mimicking the app's runtime configuration.
func persistFixture(t *testing.T) (m *Manager, eeprom, snapshot string) {
	t.Helper()
	dir := t.TempDir()
	eeprom = filepath.Join(dir, "slave-eeprom")
	snapshot = filepath.Join(dir, "efivars", "store.bin")
	// A blank EEPROM reads as 0xff (erased flash) — no magic.
	if err := os.WriteFile(eeprom, bytes.Repeat([]byte{0xff}, 32768), 0o600); err != nil {
		t.Fatal(err)
	}
	m = NewManager(NewFileBackend(eeprom, 32768))
	m.snapshotPath = snapshot
	return m, eeprom, snapshot
}

// writeEEPROM lays a valid store blob at offset 0 of the fake EEPROM.
func writeEEPROM(t *testing.T, path string, vars []Variable) []byte {
	t.Helper()
	blob := Encode(vars)
	fd, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if _, err := fd.WriteAt(blob, 0); err != nil {
		t.Fatal(err)
	}
	return blob
}

// Data on the EEPROM at startup, no snapshot yet → the app persists it.
func TestReconcilePersistsExistingEEPROM(t *testing.T) {
	m, eeprom, snapshot := persistFixture(t)
	want := writeEEPROM(t, eeprom, sampleVars())

	m.mu.Lock()
	m.reconcileLocked()
	m.mu.Unlock()

	got, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("snapshot = %d bytes, want %d bytes matching the EEPROM", len(got), len(want))
	}
}

// Blank EEPROM at startup with a saved snapshot → restore it into the EEPROM
// so the host reads its prior variables (the cross-BMC-reboot case).
func TestReconcileRestoresSnapshotToBlankEEPROM(t *testing.T) {
	m, eeprom, snapshot := persistFixture(t)
	want := Encode(sampleVars())
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, want, 0o600); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.reconcileLocked()
	m.mu.Unlock()

	// The EEPROM must now decode to the same variables.
	blob, state := readBlob(NewFileBackend(eeprom, 32768))
	if state != blobValid {
		t.Fatalf("EEPROM state after restore = %v, want valid", state)
	}
	if !bytes.Equal(blob, want) {
		t.Errorf("restored EEPROM blob mismatch")
	}
}

// Blank EEPROM and no snapshot → nothing is created; the watcher will persist
// once the host first populates the store.
func TestReconcileNoDataNoSnapshot(t *testing.T) {
	m, _, snapshot := persistFixture(t)

	m.mu.Lock()
	m.reconcileLocked()
	m.mu.Unlock()

	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Errorf("snapshot created from a blank store (err=%v)", err)
	}
}

// The host populates the EEPROM after startup → the next poll persists it.
func TestPollPersistsWhenDataAppears(t *testing.T) {
	m, eeprom, snapshot := persistFixture(t)

	// Startup: nothing yet.
	m.mu.Lock()
	m.reconcileLocked()
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Fatalf("snapshot created too early")
	}
	m.mu.Unlock()

	// Host (U-Boot) writes the store for the first time.
	want := writeEEPROM(t, eeprom, sampleVars())

	m.mu.Lock()
	persisted := m.pollLocked()
	m.mu.Unlock()
	if !persisted {
		t.Fatal("pollLocked did not persist a freshly-populated store")
	}
	got, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("snapshot mismatch after first population")
	}

	// A second poll with no change must be a no-op.
	m.mu.Lock()
	again := m.pollLocked()
	m.mu.Unlock()
	if again {
		t.Error("pollLocked re-persisted an unchanged store")
	}
}

// A change to the store (e.g. host clears BootNext) is re-persisted.
func TestPollPersistsChanges(t *testing.T) {
	m, eeprom, snapshot := persistFixture(t)
	writeEEPROM(t, eeprom, sampleVars())
	m.mu.Lock()
	m.reconcileLocked() // establishes the initial snapshot
	m.mu.Unlock()

	// Host rewrites the store with one fewer variable.
	want := writeEEPROM(t, eeprom, sampleVars()[:1])

	m.mu.Lock()
	persisted := m.pollLocked()
	m.mu.Unlock()
	if !persisted {
		t.Fatal("pollLocked did not persist a changed store")
	}
	got, _ := os.ReadFile(snapshot)
	if !bytes.Equal(got, want) {
		t.Errorf("snapshot not updated to the changed store")
	}
}

// The app's own writes (Set) persist to the snapshot immediately.
func TestSetPersistsThroughManager(t *testing.T) {
	m, _, snapshot := persistFixture(t)

	if err := m.Set(Variable{
		GUID:       EFIGlobalVariable,
		Name:       "BootNext",
		Attributes: AttrBootVariable,
		Data:       []byte{0x02, 0x00},
	}); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	blob, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("Set() did not persist to snapshot: %v", err)
	}
	vars, err := Decode(blob)
	if err != nil {
		t.Fatalf("snapshot blob invalid: %v", err)
	}
	if len(vars) != 1 || vars[0].Name != "BootNext" {
		t.Errorf("snapshot vars = %+v, want a single BootNext", vars)
	}
}

// A corrupt snapshot must never be restored into the EEPROM.
func TestReconcileIgnoresCorruptSnapshot(t *testing.T) {
	m, eeprom, snapshot := persistFixture(t)
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid magic/length header but a broken CRC/body.
	bad := Encode(sampleVars())
	bad[len(bad)-1] ^= 0xff
	if err := os.WriteFile(snapshot, bad, 0o600); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.reconcileLocked()
	m.mu.Unlock()

	if _, state := readBlob(NewFileBackend(eeprom, 32768)); state != blobBlank {
		t.Errorf("EEPROM state = %v, want blank (corrupt snapshot must not restore)", state)
	}
}
