// Package bootloader reads the running Raspberry Pi 5 bootloader's provenance
// from UEFI variables that U-Boot publishes into the shared I2C variable
// store.
//
// U-Boot's board code (board/raspberrypi/rpi/rpi.c, rpi_publish_bootloader_vars)
// mirrors facts out of the VPU firmware device tree, gated on
// /chosen/bootloader/update-timestamp so the EEPROM is only rewritten when the
// bootloader actually changes:
//
//	BootloaderConfig           the live EEPROM bootconf text (blconfig nvmem region)
//	BootloaderUpdateTimestamp  the EEPROM flash time, u32 LE seconds since epoch
//	BootloaderVersion          legacy: the rpi-eeprom git hash
//
// BootloaderVersion is no longer published: the bootloader's version and git
// hash now ride in the SMBIOS Type 45 firmware inventory, whose free-form
// string fields carry them exactly (see pkg/smbios). It is still
// read here so a board running older U-Boot keeps reporting a hash.
//
// All share one vendor GUID, defined identically here and in U-Boot. Reading
// them over I2C lets the BMC report the running bootloader's config without
// pulling the 2 MiB pieeprom.bin off the USB gadget, and the timestamp tells
// the BMC when that image has changed so it can avoid re-parsing an unchanged
// one.
package bootloader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
)

// VendorGUID is the vendor GUID U-Boot tags these variables with. It must
// stay byte-for-byte identical to the EFI_GUID() in
// board/raspberrypi/rpi/rpi.c — efivars.MustParseGUID and U-Boot's EFI_GUID
// macro both produce the mixed-endian binary form, so the same canonical
// string yields the same bytes on each side.
var VendorGUID = efivars.MustParseGUID("d1a0f2c4-9b3e-4f7a-8c21-6e5b0a7d4f10")

// UEFI variable names, matching the u"..." literals in U-Boot. VarVersion is
// legacy — current U-Boot reports the version via SMBIOS Type 45 instead.
const (
	VarVersion         = "BootloaderVersion"
	VarConfig          = "BootloaderConfig"
	VarUpdateTimestamp = "BootloaderUpdateTimestamp"
)

// ErrNotConfigured is returned when the UEFI variable store is unavailable
// (efivars disabled or no backend). Callers treat it as "fall back to the
// pieeprom.bin image," not as a hard error.
var ErrNotConfigured = errors.New("bootloader: UEFI variable store not available")

// Info is the running bootloader's provenance.
type Info struct {
	// Version is the rpi-eeprom git hash, e.g.
	// "086b83e3332dfc8927c56762771d082f3077a1ae". This is distinct from the
	// build-date "version" the firmware package parses out of pieeprom.bin —
	// it exists nowhere in the image, only in the firmware device tree.
	//
	// Legacy: current U-Boot reports the hash as the SMBIOS type 45 firmware
	// ID instead, so this is empty there. See firmware.BootloaderProvenance,
	// which prefers the SMBIOS value and falls back to this one.
	Version string
	// Config is the live EEPROM configuration text (the bootconf.txt the
	// running bootloader is using), copied from the blconfig nvmem region.
	Config string
	// UpdateTimestamp is when the EEPROM was last flashed, as reported by the
	// firmware. Zero when the variable is absent.
	UpdateTimestamp uint32
	// UpdatedAt is UpdateTimestamp as a time; zero when unset.
	UpdatedAt time.Time
}

// reader is the subset of *efivars.Manager this package needs, so tests can
// substitute a fake.
type reader interface {
	Available() bool
	Get(guid efivars.GUID, name string) (*efivars.Variable, error)
}

// Store reads and caches the bootloader variables. It is safe for concurrent
// use. The cache is keyed on UpdateTimestamp: a read that finds the same
// timestamp serves the cached Info without re-fetching the (larger) config
// and version variables.
type Store struct {
	mu       sync.Mutex
	mgr      reader
	cache    *Info
	cachedTS uint32
	hasCache bool
}

// NewStore returns a Store over the given variable reader.
func NewStore(mgr reader) *Store { return &Store{mgr: mgr} }

// Available reports whether the underlying variable store is usable.
func (s *Store) Available() bool {
	return s != nil && s.mgr != nil && s.mgr.Available()
}

// Load returns the current bootloader info. It reads the update-timestamp
// first and serves a cached result when it is unchanged, so the common
// steady-state call is a single small variable read.
func (s *Store) Load() (*Info, error) {
	if !s.Available() {
		return nil, ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tsVar, err := s.mgr.Get(VendorGUID, VarUpdateTimestamp)
	if err != nil {
		return nil, err
	}
	ts := decodeTimestamp(tsVar)

	if s.hasCache && s.cachedTS == ts {
		cp := *s.cache
		return &cp, nil
	}

	info := &Info{UpdateTimestamp: ts}
	if ts != 0 {
		info.UpdatedAt = time.Unix(int64(ts), 0).UTC()
	}

	if v, err := s.mgr.Get(VendorGUID, VarVersion); err != nil {
		return nil, err
	} else if v != nil {
		info.Version = decodeString(v.Data)
	}

	if v, err := s.mgr.Get(VendorGUID, VarConfig); err != nil {
		return nil, err
	} else if v != nil {
		info.Config = decodeString(v.Data)
	}

	s.cache, s.cachedTS, s.hasCache = info, ts, true
	cp := *info
	return &cp, nil
}

// Timestamp returns just the update-timestamp, the cheap signal callers use
// to decide whether a cached pieeprom.bin parse is still current. Returns 0
// when the variable is absent and ErrNotConfigured when the store is down.
func (s *Store) Timestamp() (uint32, error) {
	if !s.Available() {
		return 0, ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.mgr.Get(VendorGUID, VarUpdateTimestamp)
	if err != nil {
		return 0, err
	}
	return decodeTimestamp(v), nil
}

// decodeTimestamp reads the 4-byte little-endian u32 U-Boot writes. A missing
// or short variable reads as 0.
func decodeTimestamp(v *efivars.Variable) uint32 {
	if v == nil || len(v.Data) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(v.Data[:4])
}

// decodeString renders variable bytes as text: U-Boot writes the version with
// a trailing NUL and the config as a possibly NUL-padded region, so trim
// trailing NULs (and surrounding whitespace) rather than keeping embedded
// zeros in a Go string.
func decodeString(b []byte) string {
	b = bytes.TrimRight(b, "\x00")
	return string(bytes.TrimRight(b, "\x00\r\n\t "))
}
