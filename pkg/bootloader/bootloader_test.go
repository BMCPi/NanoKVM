package bootloader

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
)

// fakeReader stands in for *efivars.Manager.
type fakeReader struct {
	avail bool
	vars  map[string]*efivars.Variable
	gets  int
}

func (f *fakeReader) Available() bool { return f.avail }

func (f *fakeReader) Get(guid efivars.GUID, name string) (*efivars.Variable, error) {
	f.gets++
	if guid != VendorGUID {
		return nil, nil
	}
	return f.vars[name], nil
}

func tsVar(ts uint32) *efivars.Variable {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, ts)
	return &efivars.Variable{GUID: VendorGUID, Name: VarUpdateTimestamp, Data: b}
}

func nulString(s string) []byte { return append([]byte(s), 0) }

func newFake(ts uint32, version, config string) *fakeReader {
	return &fakeReader{
		avail: true,
		vars: map[string]*efivars.Variable{
			VarUpdateTimestamp: tsVar(ts),
			VarVersion:         {GUID: VendorGUID, Name: VarVersion, Data: nulString(version)},
			VarConfig:          {GUID: VendorGUID, Name: VarConfig, Data: nulString(config)},
		},
	}
}

const sampleHash = "086b83e3332dfc8927c56762771d082f3077a1ae"

// The GUID bytes must equal what U-Boot's EFI_GUID() macro emits for the same
// canonical string — the cross-repo contract. EFI_GUID lays the first three
// fields little-endian, so this is the exact mixed-endian binary form.
func TestVendorGUIDMatchesUBootLayout(t *testing.T) {
	want := efivars.GUID{
		0xc4, 0xf2, 0xa0, 0xd1, // 0xd1a0f2c4 LE
		0x3e, 0x9b, // 0x9b3e LE
		0x7a, 0x4f, // 0x4f7a LE
		0x8c, 0x21, 0x6e, 0x5b, 0x0a, 0x7d, 0x4f, 0x10, // trailing bytes as-is
	}
	if VendorGUID != want {
		t.Errorf("VendorGUID = % x, want % x (must match U-Boot EFI_GUID bytes)",
			VendorGUID[:], want[:])
	}
}

func TestLoadReadsAllThreeVariables(t *testing.T) {
	s := NewStore(newFake(1737000000, sampleHash, "[all]\nBOOT_ORDER=0xf41\n"))

	info, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Version != sampleHash {
		t.Errorf("Version = %q, want %q", info.Version, sampleHash)
	}
	if info.Config != "[all]\nBOOT_ORDER=0xf41" {
		t.Errorf("Config = %q", info.Config)
	}
	if info.UpdateTimestamp != 1737000000 {
		t.Errorf("UpdateTimestamp = %d, want 1737000000", info.UpdateTimestamp)
	}
	if info.UpdatedAt.IsZero() || info.UpdatedAt.Unix() != 1737000000 {
		t.Errorf("UpdatedAt = %v, want unix 1737000000", info.UpdatedAt)
	}
}

// U-Boot writes the version with a trailing NUL and the config as a possibly
// NUL-padded nvmem region; both must come back as clean Go strings.
func TestLoadTrimsNulPaddingAndWhitespace(t *testing.T) {
	f := newFake(42, sampleHash, "cfg")
	f.vars[VarConfig].Data = []byte("BOOT_ORDER=0xf41\n\x00\x00\x00\x00")
	f.vars[VarVersion].Data = []byte(sampleHash + "\x00")

	info, err := NewStore(f).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Config != "BOOT_ORDER=0xf41" {
		t.Errorf("Config = %q, want trailing NUL/whitespace trimmed", info.Config)
	}
	if info.Version != sampleHash {
		t.Errorf("Version = %q", info.Version)
	}
}

// A steady-state read (same timestamp) must serve the cache: one variable read
// (the timestamp) rather than three.
func TestLoadCachesByTimestamp(t *testing.T) {
	f := newFake(100, sampleHash, "cfg")
	s := NewStore(f)

	if _, err := s.Load(); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if f.gets != 3 {
		t.Fatalf("first Load did %d gets, want 3 (ts+version+config)", f.gets)
	}
	if _, err := s.Load(); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if f.gets != 4 {
		t.Errorf("second Load did %d total gets, want 4 (cache hit reads only the timestamp)", f.gets)
	}
}

// When the EEPROM is reflashed the timestamp advances, and the store must
// re-read version and config rather than serve the stale cache.
func TestLoadRefetchesWhenTimestampAdvances(t *testing.T) {
	f := newFake(100, "oldhash", "oldcfg")
	s := NewStore(f)

	if _, err := s.Load(); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	// Simulate a flash: new timestamp + new values.
	f.vars[VarUpdateTimestamp] = tsVar(200)
	f.vars[VarVersion].Data = nulString("newhash")
	f.vars[VarConfig].Data = nulString("newcfg")

	info, err := s.Load()
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if info.Version != "newhash" || info.Config != "newcfg" || info.UpdateTimestamp != 200 {
		t.Errorf("stale cache served after timestamp advance: %+v", info)
	}
}

func TestLoadUnavailable(t *testing.T) {
	s := NewStore(&fakeReader{avail: false})
	if _, err := s.Load(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Load(unavailable) = %v, want ErrNotConfigured", err)
	}
	var nilStore *Store
	if nilStore.Available() {
		t.Error("nil store reports Available")
	}
	if _, err := nilStore.Load(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Load(nil) = %v, want ErrNotConfigured", err)
	}
}

// Absent variables read as empty, not an error — a board that hasn't published
// yet must degrade gracefully.
func TestLoadMissingVariables(t *testing.T) {
	f := &fakeReader{avail: true, vars: map[string]*efivars.Variable{}}
	info, err := NewStore(f).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Version != "" || info.Config != "" || info.UpdateTimestamp != 0 {
		t.Errorf("expected empty Info for a store with no variables, got %+v", info)
	}
	if !info.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt should be zero when the timestamp is absent")
	}
}

func TestTimestamp(t *testing.T) {
	s := NewStore(newFake(1737000000, sampleHash, "cfg"))
	ts, err := s.Timestamp()
	if err != nil {
		t.Fatalf("Timestamp: %v", err)
	}
	if ts != 1737000000 {
		t.Errorf("Timestamp = %d, want 1737000000", ts)
	}
}

// A short/garbage timestamp variable must read as 0, not panic.
func TestDecodeTimestampShort(t *testing.T) {
	if got := decodeTimestamp(&efivars.Variable{Data: []byte{0x01, 0x02}}); got != 0 {
		t.Errorf("decodeTimestamp(short) = %d, want 0", got)
	}
	if got := decodeTimestamp(nil); got != 0 {
		t.Errorf("decodeTimestamp(nil) = %d, want 0", got)
	}
}
