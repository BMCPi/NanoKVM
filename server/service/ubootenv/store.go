package ubootenv

// store.go backs the U-Boot environment with a region of the I2C EEPROM
// rather than a file inside the boot image.
//
// U-Boot (CONFIG_ENV_IS_IN_EEPROM) keeps its environment at a fixed offset of
// the same 24c256 that holds the UEFI variable store and the SMBIOS tables:
//
//	0x0000..0x3fff  UEFI variable blob (see the efivars package)
//	0x4000..0x5fff  U-Boot environment (this store)
//	0x6000..0x67ff  SMBIOS tables (see the smbios package)
//
// The region is a complete env partition in U-Boot's binary format: a 4-byte
// little-endian CRC32 followed by NUL-terminated key=value entries and NUL
// padding. The CRC covers the entire payload *including* the padding, so
// reads and writes always span the full region — there is no length header to
// short-circuit on, unlike the UEFI blob.
//
// That also makes the region size load-bearing rather than a mere bound: it
// must equal the host's CONFIG_ENV_SIZE, because both sides checksum
// size-4 bytes. A size that disagrees makes U-Boot reject an intact
// environment with "bad CRC, using default environment"; config clamps the
// value at the SMBIOS offset to keep the two in step.
//
// The EEPROM the host talks to is emulated by the kernel i2c-slave-eeprom
// driver, whose buffer is volatile RAM wiped on every BMC boot. As in
// efivars, a durable snapshot on the data partition is reconciled at startup and kept in
// sync with host-side (saveenv) writes so the environment survives BMC
// reboots.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Backend reads and writes the raw bytes backing the store, using absolute
// offsets into the underlying device. The efivars file and I2C backends
// satisfy it structurally, so both stores can share one EEPROM device without
// this package depending on efivars.
type Backend interface {
	ReadAt(off int, p []byte) error
	WriteAt(off int, p []byte) error
	Size() int
}

const (
	// storeCacheTTL bounds how long a parsed env is served without re-reading
	// the EEPROM, so dashboard polling does not hit the device every request.
	storeCacheTTL = 2 * time.Second
	// storePollInterval bounds how quickly a host-side (saveenv) write is
	// captured into the durable snapshot.
	storePollInterval = 5 * time.Second
)

// ErrNotConfigured is returned by a Store with no backend wired up.
var ErrNotConfigured = errors.New("ubootenv: store not configured")

// Store is a U-Boot environment held in a fixed [offset, offset+size) region
// of an EEPROM. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	backend Backend
	offset  int
	size    int

	cache     *Env
	cacheTime time.Time

	// snapshotPath is the durable data-partition mirror of the region; empty disables
	// persistence. lastSnapshot is the blob most recently written there, so
	// the watcher and Save skip redundant rewrites.
	snapshotPath string
	lastSnapshot []byte
	persistOnce  sync.Once
}

// NewStore returns a Store over the given EEPROM region. snapshotPath may be
// empty to disable durable persistence.
func NewStore(b Backend, offset, size int, snapshotPath string) *Store {
	if size <= 0 {
		size = DefaultEnvSize
	}
	return &Store{backend: b, offset: offset, size: size, snapshotPath: snapshotPath}
}

// Available reports whether a backend is configured.
func (s *Store) Available() bool { return s != nil && s.backend != nil }

// Size returns the env region size in bytes.
func (s *Store) Size() int { return s.size }

// Invalidate drops the cached env, forcing the next read to hit the EEPROM.
// Call when the host may have rewritten the environment (e.g. after a boot).
func (s *Store) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheTime = time.Time{}
}

// Load returns a copy of the current environment. A blank or CRC-invalid
// region reads as an empty environment rather than an error — U-Boot itself
// falls back to its built-in defaults in that case, and a first boot must work.
func (s *Store) Load() (*Env, error) {
	if !s.Available() {
		return nil, ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	env, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	return env.clone(), nil
}

// Update applies fn to the current environment and writes the result back.
func (s *Store) Update(fn func(*Env)) error {
	if !s.Available() {
		return ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	env, err := s.loadLocked()
	if err != nil {
		return err
	}
	next := env.clone()
	fn(next)
	return s.saveLocked(next)
}

// Save replaces the entire environment.
func (s *Store) Save(env *Env) error {
	if !s.Available() {
		return ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(env.clone())
}

// loadLocked reads and parses the region, serving a fresh cache when possible.
// Must hold s.mu.
func (s *Store) loadLocked() (*Env, error) {
	if s.cache != nil && !s.cacheTime.IsZero() && time.Since(s.cacheTime) < storeCacheTTL {
		return s.cache, nil
	}
	raw, err := s.readRegion()
	if err != nil {
		return nil, err
	}
	env, ok := tryParseBinary(raw)
	if !ok {
		env = NewBinary(s.size)
	}
	s.cache, s.cacheTime = env, time.Now()
	return env, nil
}

// saveLocked marshals env and writes the whole region. Must hold s.mu.
func (s *Store) saveLocked(env *Env) error {
	env.Format = FormatBinary
	env.Size = s.size
	blob, err := env.MarshalBinary(s.size)
	if err != nil {
		return err
	}
	if err := s.writeRegion(blob); err != nil {
		s.cacheTime = time.Time{}
		return err
	}
	s.cache, s.cacheTime = env, time.Now()
	s.persistLocked(blob)
	return nil
}

func (s *Store) readRegion() ([]byte, error) {
	buf := make([]byte, s.size)
	if err := s.backend.ReadAt(s.offset, buf); err != nil {
		return nil, fmt.Errorf("ubootenv: read env region at %#x: %w", s.offset, err)
	}
	return buf, nil
}

func (s *Store) writeRegion(blob []byte) error {
	if len(blob) != s.size {
		return fmt.Errorf("ubootenv: env blob is %d bytes, region is %d", len(blob), s.size)
	}
	if err := s.backend.WriteAt(s.offset, blob); err != nil {
		return fmt.Errorf("ubootenv: write env region at %#x: %w", s.offset, err)
	}
	return nil
}

// ---- durable snapshot ------------------------------------------------------

// StartPersistence reconciles the durable snapshot against the EEPROM once,
// then launches a watcher that captures host-side (saveenv) writes. Safe to
// call on an unconfigured store or without a snapshot path (both no-op), and
// idempotent — only the first call has an effect.
func (s *Store) StartPersistence() {
	if s == nil {
		return
	}
	s.persistOnce.Do(func() {
		if s.backend == nil || s.snapshotPath == "" {
			return
		}
		s.mu.Lock()
		s.reconcileLocked()
		s.mu.Unlock()

		go s.watch()
	})
}

// reconcileLocked mirrors a valid EEPROM env to the snapshot, or restores the
// snapshot into a blank EEPROM. Must hold s.mu.
func (s *Store) reconcileLocked() {
	raw, err := s.readRegion()
	if err != nil {
		log.Warnf("ubootenv: EEPROM not readable at startup, deferring to watcher: %v", err)
		return
	}
	snap := readSnapshot(s.snapshotPath, s.size)

	if _, ok := tryParseBinary(raw); ok {
		// The EEPROM holds a valid env and is authoritative — U-Boot may have
		// just rewritten it via saveenv.
		if !bytes.Equal(raw, snap) {
			if err := s.writeSnapshotLocked(raw); err != nil {
				log.Warnf("ubootenv: persisting env from EEPROM: %v", err)
				return
			}
			log.Infof("ubootenv: persisted %d-byte env from EEPROM to %s", len(raw), s.snapshotPath)
		}
		s.lastSnapshot = raw
		return
	}

	if snap == nil {
		log.Info("ubootenv: no persisted env yet; will persist once the host populates the EEPROM")
		return
	}

	// The volatile EEPROM came up blank after a BMC reboot; restore the prior
	// environment so the host finds it.
	if err := s.writeRegion(snap); err != nil {
		log.Warnf("ubootenv: restoring env to EEPROM: %v", err)
		return
	}
	s.cacheTime = time.Time{} // force a re-read of the restored bytes
	s.lastSnapshot = snap
	log.Infof("ubootenv: restored %d-byte env to EEPROM from %s", len(snap), s.snapshotPath)
}

// watch polls the EEPROM and persists any fresh, valid env that differs from
// the last snapshot — this captures U-Boot's saveenv writes.
func (s *Store) watch() {
	ticker := time.NewTicker(storePollInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		s.pollLocked()
		s.mu.Unlock()
	}
}

// pollLocked reads the region once and persists it when it holds a fresh,
// valid env that differs from the last snapshot. Must hold s.mu.
func (s *Store) pollLocked() {
	raw, err := s.readRegion()
	if err != nil {
		return
	}
	if _, ok := tryParseBinary(raw); !ok {
		return
	}
	if bytes.Equal(raw, s.lastSnapshot) {
		return
	}
	if err := s.writeSnapshotLocked(raw); err != nil {
		log.Warnf("ubootenv: persisting env: %v", err)
		return
	}
	s.lastSnapshot = raw
	s.cacheTime = time.Time{} // the host changed the env; drop the stale cache
	log.Infof("ubootenv: persisted %d-byte env to %s", len(raw), s.snapshotPath)
}

// persistLocked mirrors a blob this process just wrote into the snapshot.
// Best-effort: a snapshot failure must not fail the EEPROM write.
// Must hold s.mu.
func (s *Store) persistLocked(blob []byte) {
	if s.snapshotPath == "" || bytes.Equal(blob, s.lastSnapshot) {
		return
	}
	if err := s.writeSnapshotLocked(blob); err != nil {
		log.Warnf("ubootenv: persisting env after write: %v", err)
		return
	}
	s.lastSnapshot = append([]byte(nil), blob...)
}

// writeSnapshotLocked writes the blob to snapshotPath atomically (temp file +
// rename) so a crash mid-write cannot corrupt the durable copy.
// Must hold s.mu.
func (s *Store) writeSnapshotLocked(blob []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.snapshotPath), 0o755); err != nil {
		return err
	}
	tmp := s.snapshotPath + ".tmp"
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
	return os.Rename(tmp, s.snapshotPath)
}

// readSnapshot reads and validates the durable snapshot. Returns nil when the
// file is missing, the wrong size, or fails its CRC — a bad snapshot must
// never be restored into the EEPROM.
func readSnapshot(path string, size int) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warnf("ubootenv: reading snapshot %s: %v", path, err)
		}
		return nil
	}
	if len(data) != size {
		log.Warnf("ubootenv: ignoring snapshot %s: %d bytes, expected %d", path, len(data), size)
		return nil
	}
	if _, ok := tryParseBinary(data); !ok {
		log.Warnf("ubootenv: ignoring corrupt snapshot %s", path)
		return nil
	}
	return data
}
