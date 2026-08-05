// Package blkinfo reads the host's block-device inventory from a fixed
// region of the shared I2C EEPROM.
//
// SMBIOS (DSP0134) defines no structure type for a disk, so the host's
// U-Boot writes this region itself (lib/blkinfo_i2c.c in the u-boot fork,
// CONFIG_BLKINFO_I2C_STORE) from its EVT_POST_PREBOOT spy — after
// "pci enum; nvme scan; usb start" — as the storage sibling of the SMBIOS
// tables at the next offset of the same EEPROM:
//
//	0x0000  UEFI variables
//	0x4000  U-Boot environment
//	0x6000  SMBIOS tables
//	0x6800  block inventory (this package)
//
// Region layout: "BLK1" magic, u16 little-endian JSON length, then JSON:
//
//	{"v":1,"drives":[{"if":"nvme","dev":0,"vendor":"..","product":"..",
//	                  "rev":"..","removable":0,"size":<bytes>}, ...]}
//
// Like SMBIOS identity, this is a boot-time snapshot: it refreshes when the
// host boots and stays readable while the host is off or wedged. The list
// is the host's honest view of its buses, so the drives the BMC itself
// serves over the USB gadget appear too.
package blkinfo

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/server/config"
	"github.com/pi-bmc/nanokvm-app/server/service/efivars"
)

// Backend reads the raw bytes backing the store using absolute offsets into
// the device — the same structural interface the smbios package uses, so
// every store shares one EEPROM device.
type Backend interface {
	ReadAt(off int, p []byte) error
	Size() int
}

const (
	// cacheTTL bounds how long a parsed inventory is served without
	// re-reading the EEPROM. The host only rewrites it at boot, so this
	// can be generous.
	cacheTTL = 30 * time.Second

	// hdrLen is the 4-byte magic plus the u16le JSON length.
	hdrLen = 6
)

var magic = []byte("BLK1")

var (
	// ErrNotConfigured is returned by a Store with no backend wired up.
	ErrNotConfigured = errors.New("blkinfo: store not configured")
	// ErrNoInventory is returned when the region holds no valid blob —
	// the host has not booted a blkinfo-capable U-Boot yet.
	ErrNoInventory = errors.New("blkinfo: no inventory in the region")
)

// Drive is one block device as the host's U-Boot reported it.
type Drive struct {
	Interface string `json:"if"`
	Devnum    int    `json:"dev"`
	Vendor    string `json:"vendor"`
	Product   string `json:"product"`
	Revision  string `json:"rev"`
	Removable int    `json:"removable"`
	SizeBytes uint64 `json:"size"`
}

// Inventory is the decoded region payload.
type Inventory struct {
	Version int     `json:"v"`
	Drives  []Drive `json:"drives"`
}

// Store reads the inventory from a fixed [offset, offset+size) EEPROM
// region. Safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	backend Backend
	offset  int
	size    int

	cache     *Inventory
	cacheTime time.Time
}

var (
	instance *Store
	once     sync.Once
)

// GetStore returns the singleton Store, wiring the backend from config on
// first use. Non-nil even when unconfigured (Load then reports it).
func GetStore() *Store {
	once.Do(func() {
		cfg := config.GetInstance().BlkInfo
		instance = &Store{}
		if !cfg.Enabled {
			return
		}

		var b Backend
		switch {
		case cfg.Path != "":
			b = efivars.NewFileBackend(cfg.Path, cfg.Offset+cfg.Size)
			log.Infof("blkinfo: using file store %s at offset %#x", cfg.Path, cfg.Offset)
		case cfg.I2CBus >= 0:
			b = efivars.NewI2CBackend(cfg.I2CBus, uint16(cfg.I2CAddr), //nolint:gosec // 7-bit address
				cfg.PageSize, cfg.Offset+cfg.Size)
			log.Infof("blkinfo: using i2c store bus %d addr %#x at offset %#x",
				cfg.I2CBus, cfg.I2CAddr, cfg.Offset)
		default:
			log.Warn("blkinfo: enabled but neither path nor i2c bus configured")
			return
		}

		instance.backend = b
		instance.offset = cfg.Offset
		instance.size = cfg.Size
	})
	return instance
}

// NewStore returns a Store over the given EEPROM region (for tests).
func NewStore(b Backend, offset, size int) *Store {
	return &Store{backend: b, offset: offset, size: size}
}

// Available reports whether a backend is configured.
func (s *Store) Available() bool { return s != nil && s.backend != nil }

// Invalidate drops the cache, forcing the next Load to hit the EEPROM.
func (s *Store) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheTime = time.Time{}
}

// Load returns the parsed inventory. It reports ErrNoInventory when the
// region is blank or invalid, so callers can degrade gracefully.
func (s *Store) Load() (*Inventory, error) {
	if !s.Available() {
		return nil, ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil && !s.cacheTime.IsZero() && time.Since(s.cacheTime) < cacheTTL {
		return s.cache, nil
	}

	raw := make([]byte, s.size)
	if err := s.backend.ReadAt(s.offset, raw); err != nil {
		return nil, fmt.Errorf("blkinfo: read region at %#x: %w", s.offset, err)
	}

	inv, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	s.cache, s.cacheTime = inv, time.Now()
	return inv, nil
}

// Parse decodes a raw region: "BLK1", u16le length, JSON. Exported so the
// region can be parsed from a file or a test fixture.
func Parse(raw []byte) (*Inventory, error) {
	if len(raw) < hdrLen || string(raw[:4]) != string(magic) {
		return nil, ErrNoInventory
	}
	n := int(binary.LittleEndian.Uint16(raw[4:6]))
	if n == 0 || hdrLen+n > len(raw) {
		return nil, fmt.Errorf("blkinfo: bogus payload length %d for a %d-byte region", n, len(raw))
	}
	var inv Inventory
	if err := json.Unmarshal(raw[hdrLen:hdrLen+n], &inv); err != nil {
		return nil, fmt.Errorf("blkinfo: decode payload: %w", err)
	}
	return &inv, nil
}
