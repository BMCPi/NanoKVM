package efivars

// manager.go is the store manager: it reads the blob through a Backend,
// caches a parsed snapshot, and provides variable CRUD plus boot manager
// helpers (BootOrder / BootNext / Boot#### entries).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// cacheTTL bounds how long a parsed snapshot is served without re-reading
// the store. EEPROM reads over a 100 kHz bus are slow (~1 ms/byte), so
// dashboard polling must not hit the hardware on every request.
const cacheTTL = 2 * time.Second

// Manager provides serialized access to the variable store.
type Manager struct {
	mu      sync.Mutex
	backend Backend

	cache     []Variable
	cacheTime time.Time

	// snapshotPath is the durable data-partition mirror of the store; see persist.go.
	// Empty disables persistence. lastSnapshot is the blob most recently
	// written there, so the watcher and save() skip redundant rewrites.
	snapshotPath string
	lastSnapshot []byte
	persistOnce  sync.Once
}

var (
	instance *Manager
	once     sync.Once
)

// GetManager returns the singleton Manager, initializing it from config on
// first call. The manager is non-nil even when unconfigured; use Available.
func GetManager() *Manager {
	once.Do(func() {
		cfg := config.GetInstance().EfiVars
		instance = &Manager{}
		if !cfg.Enabled {
			return
		}
		instance.snapshotPath = cfg.SnapshotPath
		switch {
		case cfg.Path != "":
			instance.backend = NewFileBackend(cfg.Path, cfg.StoreSize)
			log.Infof("efivars: using file store %s", cfg.Path)
		case cfg.I2CBus >= 0:
			instance.backend = NewI2CBackend(cfg.I2CBus, uint16(cfg.I2CAddr), //nolint:gosec // 7-bit address
				cfg.PageSize, cfg.StoreSize)
			log.Infof("efivars: using i2c store bus %d addr %#x", cfg.I2CBus, cfg.I2CAddr)
		default:
			log.Warn("efivars: enabled but neither path nor i2c bus configured")
		}
	})
	return instance
}

// NewManager returns a Manager over an explicit backend (for tests and
// tooling; the app uses GetManager).
func NewManager(b Backend) *Manager {
	return &Manager{backend: b}
}

// Available reports whether a store backend is configured.
func (m *Manager) Available() bool {
	return m != nil && m.backend != nil
}

// Invalidate drops the cached snapshot, forcing the next read to hit the
// store. Call after the host may have modified variables (e.g. host boot).
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheTime = time.Time{}
}

// load returns the parsed variables, from cache when fresh. Must hold m.mu.
// A blank store (no magic) is returned as an empty variable list.
func (m *Manager) load() ([]Variable, error) {
	if m.backend == nil {
		return nil, errors.New("efivars: store not configured")
	}
	if !m.cacheTime.IsZero() && time.Since(m.cacheTime) < cacheTTL {
		return m.cache, nil
	}

	hdr := make([]byte, headerSize)
	if err := m.backend.ReadAt(0, hdr); err != nil {
		return nil, err
	}
	length, err := DecodeHeader(hdr)
	if errors.Is(err, ErrNoStore) {
		m.cache, m.cacheTime = nil, time.Now()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if size := m.backend.Size(); size > 0 && length > size {
		return nil, fmt.Errorf("%w: length %d exceeds store size %d", ErrCorrupt, length, size)
	}

	blob := make([]byte, length)
	copy(blob, hdr)
	if length > headerSize {
		if err := m.backend.ReadAt(headerSize, blob[headerSize:]); err != nil {
			return nil, err
		}
	}
	vars, err := Decode(blob)
	if err != nil {
		return nil, err
	}
	m.cache, m.cacheTime = vars, time.Now()
	return vars, nil
}

// save serializes and writes the variables, then refreshes the cache.
// Must hold m.mu.
func (m *Manager) save(vars []Variable) error {
	blob := Encode(vars)
	if size := m.backend.Size(); size > 0 && len(blob) > size {
		return fmt.Errorf("efivars: blob (%d bytes) exceeds store size (%d)", len(blob), size)
	}
	if err := m.backend.WriteAt(0, blob); err != nil {
		m.cacheTime = time.Time{}
		return err
	}
	m.cache, m.cacheTime = vars, time.Now()
	// Mirror the just-written blob to the durable snapshot (best-effort).
	m.persistLocked(blob)
	return nil
}

// Variables returns all variables in the store.
func (m *Manager) Variables() ([]Variable, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vars, err := m.load()
	if err != nil {
		return nil, err
	}
	out := make([]Variable, len(vars))
	copy(out, vars)
	return out, nil
}

// Get returns the variable with the given vendor GUID and name.
func (m *Manager) Get(guid GUID, name string) (*Variable, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vars, err := m.load()
	if err != nil {
		return nil, err
	}
	for i := range vars {
		if vars[i].GUID == guid && vars[i].Name == name {
			v := vars[i]
			return &v, nil
		}
	}
	return nil, nil
}

// Set creates or replaces a variable and persists the store.
func (m *Manager) Set(v Variable) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	vars, err := m.load()
	if err != nil {
		return err
	}
	replaced := false
	for i := range vars {
		if vars[i].key() == v.key() {
			vars[i] = v
			replaced = true
			break
		}
	}
	if !replaced {
		vars = append(vars, v)
	}
	return m.save(vars)
}

// Delete removes a variable and persists the store. Deleting a variable
// that does not exist is a no-op.
func (m *Manager) Delete(guid GUID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	vars, err := m.load()
	if err != nil {
		return err
	}
	for i := range vars {
		if vars[i].GUID == guid && vars[i].Name == name {
			return m.save(append(vars[:i:i], vars[i+1:]...))
		}
	}
	return nil
}

// --- Boot manager helpers -------------------------------------------------

// BootEntry is a parsed Boot#### variable.
type BootEntry struct {
	// ID is the numeric suffix (Boot0001 -> 1).
	ID uint16
	// Name is the full variable name, e.g. "Boot0001".
	Name        string
	Description string
	Active      bool
	Target      BootTarget
}

// bootVarID extracts N from a "BootXXXX" hex-suffixed variable name.
func bootVarID(name string) (uint16, bool) {
	if len(name) != 8 || !strings.HasPrefix(name, "Boot") {
		return 0, false
	}
	var id uint16
	if _, err := fmt.Sscanf(name[4:], "%04X", &id); err != nil {
		return 0, false
	}
	return id, true
}

// BootEntries returns all parsed Boot#### entries sorted by ID.
func (m *Manager) BootEntries() ([]BootEntry, error) {
	vars, err := m.Variables()
	if err != nil {
		return nil, err
	}
	var entries []BootEntry
	for i := range vars {
		if vars[i].GUID != EFIGlobalVariable {
			continue
		}
		id, ok := bootVarID(vars[i].Name)
		if !ok {
			continue
		}
		opt, err := ParseLoadOption(vars[i].Data)
		if err != nil {
			log.Warnf("efivars: skipping malformed %s: %v", vars[i].Name, err)
			continue
		}
		entries = append(entries, BootEntry{
			ID:          id,
			Name:        vars[i].Name,
			Description: opt.Description,
			Active:      opt.Active(),
			Target:      opt.Target(),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

// BootOrder returns the current BootOrder ID list (empty if unset).
func (m *Manager) BootOrder() ([]uint16, error) {
	v, err := m.Get(EFIGlobalVariable, "BootOrder")
	if err != nil || v == nil {
		return nil, err
	}
	order := make([]uint16, len(v.Data)/2)
	for i := range order {
		order[i] = binary.LittleEndian.Uint16(v.Data[2*i:])
	}
	return order, nil
}

// SetBootOrder replaces BootOrder.
func (m *Manager) SetBootOrder(order []uint16) error {
	data := make([]byte, 2*len(order))
	for i, id := range order {
		binary.LittleEndian.PutUint16(data[2*i:], id)
	}
	return m.Set(Variable{
		GUID:       EFIGlobalVariable,
		Name:       "BootOrder",
		Attributes: AttrBootVariable,
		Data:       data,
	})
}

// BootNext returns the pending BootNext ID, or nil if unset.
func (m *Manager) BootNext() (*uint16, error) {
	v, err := m.Get(EFIGlobalVariable, "BootNext")
	if err != nil || v == nil {
		return nil, err
	}
	if len(v.Data) != 2 {
		return nil, fmt.Errorf("%w: BootNext has %d bytes", ErrCorrupt, len(v.Data))
	}
	id := binary.LittleEndian.Uint16(v.Data)
	return &id, nil
}

// SetBootNext sets the one-shot boot override to the given Boot#### ID.
func (m *Manager) SetBootNext(id uint16) error {
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, id)
	return m.Set(Variable{
		GUID:       EFIGlobalVariable,
		Name:       "BootNext",
		Attributes: AttrBootVariable,
		Data:       data,
	})
}

// ClearBootNext removes the one-shot boot override.
func (m *Manager) ClearBootNext() error {
	return m.Delete(EFIGlobalVariable, "BootNext")
}
