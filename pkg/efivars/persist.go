package efivars

// persist.go mirrors the variable store to a durable file on the data partition.
//
// The EEPROM the host writes is emulated by the kernel i2c-slave-eeprom
// driver, whose backing buffer is volatile RAM: it is zeroed on every BMC
// boot and has no firmware backing. Without help, the first host boot after
// each BMC restart finds a blank store ("No EFI variables found in EEPROM")
// and U-Boot rewrites defaults, losing any BootOrder/BootNext set out-of-band
// or in a prior session.
//
// This file closes that gap with a two-way mirror against SnapshotPath:
//
//   - startup reconcile (reconcileLocked):
//       * store has data  -> the EEPROM is authoritative (U-Boot may have just
//         updated it); persist it to the snapshot.
//       * store is blank + snapshot present -> restore the snapshot into the
//         EEPROM so the host reads the prior variables.
//       * store is blank + no snapshot -> nothing yet; the watcher persists
//         once the host populates the EEPROM for the first time.
//   - watcher (watch): polls the EEPROM and persists whenever a fresh, valid
//     blob appears or changes — this captures U-Boot's out-of-band writes.
//   - the app's own writes persist immediately via save() (persistLocked).

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

// pollInterval bounds how quickly a host-side (U-Boot) write to the EEPROM is
// captured to the durable snapshot. The store blob is ~1 KiB and the default
// backend reads the kernel slave-eeprom RAM buffer, so polling is cheap.
const pollInterval = 5 * time.Second

// StartPersistence reconciles the durable snapshot against the EEPROM once,
// then launches a watcher that keeps the snapshot in sync with host-side
// writes. It is safe to call on an unconfigured manager or without a snapshot
// path (both no-op), and idempotent — only the first call has an effect.
func (m *Manager) StartPersistence() {
	if m == nil {
		return
	}
	m.persistOnce.Do(func() {
		if m.backend == nil || m.snapshotPath == "" {
			return
		}
		m.mu.Lock()
		m.reconcileLocked()
		m.mu.Unlock()

		go m.watch()
	})
}

// reconcileLocked performs the one-shot startup reconciliation described in
// the file header. Must hold m.mu.
func (m *Manager) reconcileLocked() {
	eeprom, eeState := readBlob(m.backend)
	snap := readSnapshot(m.snapshotPath)

	switch eeState {
	case blobValid:
		// The EEPROM holds a valid store and is authoritative — the host may
		// have just rewritten it (e.g. cleared BootNext after honouring it).
		if !bytes.Equal(eeprom, snap) {
			if err := m.writeSnapshotLocked(eeprom); err != nil {
				log.Warnf("efivars: persisting store from EEPROM: %v", err)
				return
			}
			log.Infof("efivars: persisted %d-byte store from EEPROM to %s", len(eeprom), m.snapshotPath)
		}
		m.lastSnapshot = eeprom

	case blobBlank:
		if snap != nil {
			// Volatile EEPROM came up blank after a BMC reboot; restore the
			// prior store so the host finds its variables.
			if err := m.backend.WriteAt(0, snap); err != nil {
				log.Warnf("efivars: restoring store to EEPROM: %v", err)
				return
			}
			m.cacheTime = time.Time{} // force a re-read of the restored bytes
			m.lastSnapshot = snap
			log.Infof("efivars: restored %d-byte store to EEPROM from %s", len(snap), m.snapshotPath)
		} else {
			log.Info("efivars: no persisted store yet; will persist once the host populates the EEPROM")
		}

	default: // blobCorrupt / blobError
		// Don't restore over a store we can't read cleanly — the host may be
		// mid-write. Leave it; the watcher retries on the next poll.
		log.Warnf("efivars: EEPROM not readable at startup, deferring to watcher")
	}
}

// watch polls the EEPROM and persists any fresh, valid store that differs from
// the last snapshot. This is what captures U-Boot's first-ever population of a
// blank store, and any later host-side changes.
func (m *Manager) watch() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		m.pollLocked()
		m.mu.Unlock()
	}
}

// pollLocked reads the EEPROM once and persists it when it holds a fresh, valid
// store that differs from the last snapshot. Returns true when it persisted.
// Must hold m.mu.
func (m *Manager) pollLocked() bool {
	blob, state := readBlob(m.backend)
	if state != blobValid || bytes.Equal(blob, m.lastSnapshot) {
		return false
	}
	if err := m.writeSnapshotLocked(blob); err != nil {
		log.Warnf("efivars: persisting store: %v", err)
		return false
	}
	m.lastSnapshot = blob
	m.cacheTime = time.Time{} // host changed the store; drop stale cache
	log.Infof("efivars: persisted %d-byte store to %s", len(blob), m.snapshotPath)
	return true
}

// persistLocked mirrors a blob the app itself just wrote to the EEPROM into the
// snapshot. Best-effort: a snapshot failure must not fail the store write.
// Must hold m.mu.
func (m *Manager) persistLocked(blob []byte) {
	if m.snapshotPath == "" {
		return
	}
	if bytes.Equal(blob, m.lastSnapshot) {
		return
	}
	if err := m.writeSnapshotLocked(blob); err != nil {
		log.Warnf("efivars: persisting store after write: %v", err)
		return
	}
	m.lastSnapshot = append([]byte(nil), blob...)
}

// writeSnapshotLocked writes the blob to SnapshotPath atomically (temp file +
// rename) so a crash mid-write cannot corrupt the durable copy. Must hold m.mu.
func (m *Manager) writeSnapshotLocked(blob []byte) error {
	if err := os.MkdirAll(filepath.Dir(m.snapshotPath), 0o755); err != nil {
		return err
	}
	tmp := m.snapshotPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, m.snapshotPath)
}

// blobState classifies a read of the store.
type blobState int

const (
	blobValid   blobState = iota // present, magic + length + CRC all good
	blobBlank                    // no magic: blank or foreign EEPROM
	blobCorrupt                  // magic present but length/CRC/parse bad
	blobError                    // backend read failed
)

// readBlob reads and fully validates the store blob (through the CRC) from a
// backend, returning the exact on-store bytes and a state classification.
func readBlob(b Backend) ([]byte, blobState) {
	hdr := make([]byte, headerSize)
	if err := b.ReadAt(0, hdr); err != nil {
		return nil, blobError
	}
	length, err := DecodeHeader(hdr)
	if errors.Is(err, ErrNoStore) {
		return nil, blobBlank
	}
	if err != nil {
		return nil, blobCorrupt
	}
	if size := b.Size(); size > 0 && length > size {
		return nil, blobCorrupt
	}
	blob := make([]byte, length)
	copy(blob, hdr)
	if length > headerSize {
		if err := b.ReadAt(headerSize, blob[headerSize:]); err != nil {
			return nil, blobError
		}
	}
	if _, err := Decode(blob); err != nil {
		return nil, blobCorrupt
	}
	return blob, blobValid
}

// readSnapshot reads and validates the durable snapshot. Returns nil when the
// file is missing or fails validation (a corrupt snapshot must not be restored
// into the EEPROM).
func readSnapshot(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warnf("efivars: reading snapshot %s: %v", path, err)
		}
		return nil
	}
	if _, err := Decode(data); err != nil {
		log.Warnf("efivars: ignoring corrupt snapshot %s: %v", path, err)
		return nil
	}
	return data
}
