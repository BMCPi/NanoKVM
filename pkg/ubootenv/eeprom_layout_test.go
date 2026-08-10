package ubootenv_test

// eeprom_layout_test.go drives all three stores against one real file backend
// at the shipped offsets, the way they run on the device: efivars, ubootenv and
// smbios share the kernel i2c-slave-eeprom backing file. The unit tests in
// store_test.go use a mock backend and only guard the region *below* the env;
// this covers the file backend and the region above it, which is where the
// SMBIOS tables live.

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
	"github.com/pi-bmc/nanokvm-app/pkg/smbios"
	"github.com/pi-bmc/nanokvm-app/pkg/ubootenv"
)

// The shipped layout — these must equal the host's CONFIG_ENV_OFFSET,
// CONFIG_ENV_SIZE, CONFIG_SMBIOS_I2C_STORE_OFFSET and _SIZE.
const (
	chipSize     = 32768 // 24c256
	uefiSize     = 0x4000
	envOffset    = 0x4000
	envSize      = 0x2000
	smbiosOffset = 0x6000
	smbiosSize   = 0x800
)

// newChip creates a backing file pre-filled with a sentinel, so any byte a
// store writes outside its own region is visible.
func newChip(t *testing.T, fill byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slave-eeprom")
	buf := make([]byte, chipSize)
	for i := range buf {
		buf[i] = fill
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("seed chip: %v", err)
	}
	return path
}

func readChip(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chip: %v", err)
	}
	if len(b) != chipSize {
		t.Fatalf("chip is %d bytes, want %d", len(b), chipSize)
	}
	return b
}

// A store write must land at its absolute offset and touch nothing else — not
// the UEFI blob below it, nor the SMBIOS tables above it.
func TestEnvStoreWritesOnlyItsRegionOfTheFile(t *testing.T) {
	path := newChip(t, 0xAA)

	// Constructed exactly as firmware.newEnvStore does: the backend is capped
	// at offset+size, and the store addresses it with absolute offsets.
	store := ubootenv.NewStore(
		efivars.NewFileBackend(path, envOffset+envSize),
		envOffset, envSize,
		filepath.Join(t.TempDir(), "env.bin"),
	)

	if err := store.Update(func(e *ubootenv.Env) {
		e.Set("bootcmd", "run distro_bootcmd")
		e.Set("serial#", "79a6e38784ccd62e")
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	chip := readChip(t, path)

	for i := range uefiSize {
		if chip[i] != 0xAA {
			t.Fatalf("UEFI region clobbered at %#x: got %#x", i, chip[i])
		}
	}
	for i := smbiosOffset; i < chipSize; i++ {
		if chip[i] != 0xAA {
			t.Fatalf("SMBIOS region clobbered at %#x: got %#x — the env overran it",
				i, chip[i])
		}
	}

	// The env region must have actually changed.
	unchanged := true
	for i := envOffset; i < envOffset+envSize; i++ {
		if chip[i] != 0xAA {
			unchanged = false
			break
		}
	}
	if unchanged {
		t.Fatal("env region was never written")
	}
}

// The bytes at envOffset must be laid out exactly as U-Boot's env_import
// reads them: a 4-byte LE CRC32 over the following CONFIG_ENV_SIZE-4 bytes.
// A CRC computed over any other length is what produces
// "bad CRC, using default environment" on an otherwise intact env.
func TestEnvBytesMatchUBootCRCConvention(t *testing.T) {
	path := newChip(t, 0x00)

	store := ubootenv.NewStore(
		efivars.NewFileBackend(path, envOffset+envSize),
		envOffset, envSize,
		filepath.Join(t.TempDir(), "env.bin"),
	)
	if err := store.Update(func(e *ubootenv.Env) { e.Set("bootdelay", "2") }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	region := readChip(t, path)[envOffset : envOffset+envSize]

	storedCRC := binary.LittleEndian.Uint32(region[:4])
	// U-Boot: crc32(0, ep->data, ENV_SIZE) where ENV_SIZE = CONFIG_ENV_SIZE - 4.
	wantCRC := crc32.ChecksumIEEE(region[4:envSize])

	if storedCRC != wantCRC {
		t.Errorf("stored CRC %#08x != crc32 over the %d payload bytes (%#08x); "+
			"U-Boot would report a bad CRC", storedCRC, envSize-4, wantCRC)
	}

	// A CRC taken over the *old* 0x4000 length must not match — that is the
	// regression this pins.
	if storedCRC == crc32.ChecksumIEEE(readChip(t, path)[envOffset+4:envOffset+0x4000]) {
		t.Error("CRC matches a 0x4000-sized payload; the region size is wrong")
	}
}

// All three stores on one file must tile without touching each other.
func TestThreeStoresShareOneChipWithoutOverlap(t *testing.T) {
	path := newChip(t, 0x00)

	// SMBIOS is read-only in production (only the host writes it), so stand in
	// for the host by writing its region directly.
	smbiosBytes := make([]byte, smbiosSize)
	for i := range smbiosBytes {
		smbiosBytes[i] = 0x5B
	}
	smbiosBackend := efivars.NewFileBackend(path, smbiosOffset+smbiosSize)
	if err := smbiosBackend.WriteAt(smbiosOffset, smbiosBytes); err != nil {
		t.Fatalf("seed smbios region: %v", err)
	}

	// The UEFI blob, bounded by the clamp config applies (storeSize == envOffset).
	uefiBytes := make([]byte, uefiSize)
	for i := range uefiBytes {
		uefiBytes[i] = 0xE1
	}
	uefiBackend := efivars.NewFileBackend(path, uefiSize)
	if err := uefiBackend.WriteAt(0, uefiBytes); err != nil {
		t.Fatalf("seed uefi region: %v", err)
	}

	// Now the env store writes between them.
	store := ubootenv.NewStore(
		efivars.NewFileBackend(path, envOffset+envSize),
		envOffset, envSize,
		filepath.Join(t.TempDir(), "env.bin"),
	)
	if err := store.Update(func(e *ubootenv.Env) { e.Set("baudrate", "115200") }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	chip := readChip(t, path)
	for i := range uefiSize {
		if chip[i] != 0xE1 {
			t.Fatalf("UEFI byte %#x = %#x, want 0xE1", i, chip[i])
		}
	}
	for i := smbiosOffset; i < smbiosOffset+smbiosSize; i++ {
		if chip[i] != 0x5B {
			t.Fatalf("SMBIOS byte %#x = %#x, want 0x5B", i, chip[i])
		}
	}

	// Each store still reads back its own content.
	env, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, _ := env.Get("baudrate"); got != "115200" {
		t.Errorf("baudrate = %q, want 115200", got)
	}

	// And the SMBIOS store reads its region, not the env's.
	info, err := smbios.NewStore(smbiosBackend, smbiosOffset, smbiosSize).Load()
	// 0x5B filler is not a valid _SM3_ anchor, so this must report no tables
	// rather than mis-parsing the env region.
	if err == nil {
		t.Errorf("SMBIOS parsed filler as tables: %+v", info)
	}
}

// Reproduces the reported failure. A config carrying the pre-shrink
// ubootEnv.size (0x4000) makes the BMC checksum 0x3ffc bytes, while U-Boot —
// built with CONFIG_ENV_SIZE=0x2000 — checksums 0x1ffc. The bytes are intact
// and at the right offset, yet the host reports
// "bad CRC, using default environment". It also spills into the SMBIOS tables.
//
// This is what config.checkDefaultValue's clamp prevents; the test documents
// why that clamp is load-bearing rather than tidy-up.
func TestLegacyEnvSizeProducesTheBadCRCUBootReports(t *testing.T) {
	const legacyEnvSize = 0x4000

	path := newChip(t, 0x00)
	store := ubootenv.NewStore(
		efivars.NewFileBackend(path, envOffset+legacyEnvSize),
		envOffset, legacyEnvSize, // the stale size
		filepath.Join(t.TempDir(), "env.bin"),
	)
	if err := store.Update(func(e *ubootenv.Env) { e.Set("bootdelay", "2") }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	chip := readChip(t, path)
	storedCRC := binary.LittleEndian.Uint32(chip[envOffset : envOffset+4])

	// What U-Boot computes: crc32 over CONFIG_ENV_SIZE-4 = 0x1ffc bytes.
	ubootCRC := crc32.ChecksumIEEE(chip[envOffset+4 : envOffset+envSize])
	if storedCRC == ubootCRC {
		t.Fatal("expected the stale size to produce a CRC U-Boot rejects; " +
			"if this now passes the reproduction is stale")
	}

	// The same region also reaches past smbiosOffset. The overrun is silent
	// because env padding is NUL — it blanks the tables rather than tripping
	// anything, which is why only the CRC symptom ever surfaced.
	if envOffset+legacyEnvSize <= smbiosOffset {
		t.Fatalf("legacy region ends at %#x, expected it to overrun smbiosOffset %#x",
			envOffset+legacyEnvSize, smbiosOffset)
	}

	// With the corrected size, the same env satisfies U-Boot's check.
	path2 := newChip(t, 0x00)
	fixed := ubootenv.NewStore(
		efivars.NewFileBackend(path2, envOffset+envSize),
		envOffset, envSize,
		filepath.Join(t.TempDir(), "env.bin"),
	)
	if err := fixed.Update(func(e *ubootenv.Env) { e.Set("bootdelay", "2") }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	chip2 := readChip(t, path2)
	if binary.LittleEndian.Uint32(chip2[envOffset:envOffset+4]) !=
		crc32.ChecksumIEEE(chip2[envOffset+4:envOffset+envSize]) {
		t.Error("corrected size still fails U-Boot's CRC check")
	}
}

// The file backend must refuse a write that would run past the region it was
// given, rather than silently corrupting the neighbour above it.
func TestFileBackendRejectsOverrun(t *testing.T) {
	path := newChip(t, 0x00)
	b := efivars.NewFileBackend(path, envOffset+envSize)

	// One byte past the env region.
	if err := b.WriteAt(envOffset, make([]byte, envSize+1)); err == nil {
		t.Error("write past the region cap was accepted")
	}
	// Exactly filling the region is fine.
	if err := b.WriteAt(envOffset, make([]byte, envSize)); err != nil {
		t.Errorf("exact-size write rejected: %v", err)
	}
}
