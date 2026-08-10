package efivars

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func TestGUIDRoundTrip(t *testing.T) {
	const text = "8be4df61-93ca-11d2-aa0d-00e098032b8c"
	g, err := ParseGUID(text)
	if err != nil {
		t.Fatalf("ParseGUID() error: %v", err)
	}
	if got := g.String(); got != text {
		t.Errorf("String() = %q, want %q", got, text)
	}
	// Binary layout: data1 little-endian.
	want := [16]byte{
		0x61, 0xdf, 0xe4, 0x8b, 0xca, 0x93, 0xd2, 0x11,
		0xaa, 0x0d, 0x00, 0xe0, 0x98, 0x03, 0x2b, 0x8c,
	}
	if g != GUID(want) {
		t.Errorf("binary GUID = %x, want %x", g[:], want[:])
	}
}

func TestParseGUIDInvalid(t *testing.T) {
	for _, s := range []string{"", "not-a-guid", "8be4df61-93ca-11d2-aa0d", "8be4df6193ca11d2aa0d00e098032b8c"} {
		if _, err := ParseGUID(s); err == nil {
			t.Errorf("ParseGUID(%q) succeeded, want error", s)
		}
	}
}

func sampleVars() []Variable {
	return []Variable{
		{
			GUID:       EFIGlobalVariable,
			Name:       "BootOrder",
			Attributes: AttrBootVariable,
			Data:       []byte{0x01, 0x00, 0x00, 0x00},
		},
		{
			GUID:       EFIGlobalVariable,
			Name:       "Boot0001",
			Attributes: AttrBootVariable,
			Time:       12345,
			Data:       encodeTestLoadOption("debian", pxeDevicePath()),
		},
		{
			GUID:       MustParseGUID("00112233-4455-6677-8899-aabbccddeeff"),
			Name:       "Odd",
			Attributes: AttrNonVolatile,
			Data:       []byte{0xde, 0xad, 0xbe, 0xef, 0x01}, // odd length exercises alignment
		},
	}
}

func TestBlobRoundTrip(t *testing.T) {
	blob := Encode(sampleVars())

	if len(blob)%8 != 0 {
		t.Errorf("blob length %d not 8-byte aligned", len(blob))
	}
	vars, err := Decode(blob)
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if len(vars) != 3 {
		t.Fatalf("got %d vars, want 3", len(vars))
	}
	for i, want := range sampleVars() {
		got := vars[i]
		if got.Name != want.Name || got.GUID != want.GUID ||
			got.Attributes != want.Attributes || got.Time != want.Time ||
			!bytes.Equal(got.Data, want.Data) {
			t.Errorf("var %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	blob := Encode(sampleVars())

	if _, err := Decode(make([]byte, 64)); err == nil {
		t.Error("Decode(zeros) succeeded, want ErrNoStore")
	}

	bad := append([]byte(nil), blob...)
	bad[len(bad)-1] ^= 0xff
	if _, err := Decode(bad); err == nil {
		t.Error("Decode(corrupted payload) succeeded, want CRC error")
	}

	short := append([]byte(nil), blob...)
	binary.LittleEndian.PutUint32(short[16:20], uint32(len(blob)+64))
	if _, err := Decode(short); err == nil {
		t.Error("Decode(oversized length) succeeded, want error")
	}
}

// encodeTestLoadOption builds a minimal EFI_LOAD_OPTION.
func encodeTestLoadOption(desc string, devPath []byte) []byte {
	return EncodeLoadOption(&LoadOption{
		Attributes:  LoadOptionActive,
		Description: desc,
		DevicePath:  devPath,
	})
}

func devPathNode(typ, sub byte, payload []byte) []byte {
	n := make([]byte, 4+len(payload))
	n[0], n[1] = typ, sub
	binary.LittleEndian.PutUint16(n[2:4], uint16(len(n)))
	copy(n[4:], payload)
	return n
}

func endNode() []byte { return []byte{0x7f, 0xff, 0x04, 0x00} }

func pxeDevicePath() []byte {
	mac := devPathNode(dpTypeMessaging, dpMsgMAC, make([]byte, 33))
	return append(mac, endNode()...)
}

func hddDevicePath() []byte {
	hd := devPathNode(dpTypeMedia, dpMediaHardDrive, make([]byte, 38))
	return append(hd, endNode()...)
}

func uriDevicePath() []byte {
	uri := devPathNode(dpTypeMessaging, dpMsgURI, []byte("http://boot/x.efi"))
	return append(uri, endNode()...)
}

func TestLoadOptionParseAndClassify(t *testing.T) {
	cases := []struct {
		name   string
		path   []byte
		target BootTarget
	}{
		{"pxe", pxeDevicePath(), TargetPxe},
		{"hdd", hddDevicePath(), TargetHdd},
		{"http", uriDevicePath(), TargetUefiHttp},
		{"usb", append(devPathNode(dpTypeMessaging, dpMsgUSB, make([]byte, 2)), endNode()...), TargetCd},
		{"empty", nil, TargetUnknown},
	}
	for _, tc := range cases {
		raw := encodeTestLoadOption("entry-"+tc.name, tc.path)
		opt, err := ParseLoadOption(raw)
		if err != nil {
			t.Fatalf("%s: ParseLoadOption() error: %v", tc.name, err)
		}
		if opt.Description != "entry-"+tc.name {
			t.Errorf("%s: description = %q", tc.name, opt.Description)
		}
		if !opt.Active() {
			t.Errorf("%s: entry not active", tc.name)
		}
		if got := opt.Target(); got != tc.target {
			t.Errorf("%s: target = %q, want %q", tc.name, got, tc.target)
		}
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.bin")
	// Simulate a blank 32 KiB EEPROM.
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xff}, 32768), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewManager(NewFileBackend(path, 32768))
}

func TestManagerBlankStore(t *testing.T) {
	m := newTestManager(t)
	vars, err := m.Variables()
	if err != nil {
		t.Fatalf("Variables() on blank store: %v", err)
	}
	if len(vars) != 0 {
		t.Errorf("blank store has %d vars", len(vars))
	}
}

func TestManagerSetGetDelete(t *testing.T) {
	m := newTestManager(t)

	v := Variable{GUID: EFIGlobalVariable, Name: "Test", Attributes: AttrBootVariable, Data: []byte{1, 2, 3}}
	if err := m.Set(v); err != nil {
		t.Fatalf("Set() error: %v", err)
	}
	m.Invalidate() // force a re-read from the backing file

	got, err := m.Get(EFIGlobalVariable, "Test")
	if err != nil || got == nil {
		t.Fatalf("Get() = %v, %v", got, err)
	}
	if !bytes.Equal(got.Data, v.Data) {
		t.Errorf("Get() data = %x", got.Data)
	}

	if err := m.Delete(EFIGlobalVariable, "Test"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if got, _ := m.Get(EFIGlobalVariable, "Test"); got != nil {
		t.Errorf("variable still present after Delete()")
	}
}

func setupBootEntries(t *testing.T, m *Manager) {
	t.Helper()
	entries := []struct {
		id   uint16
		desc string
		path []byte
	}{
		{1, "disk", hddDevicePath()},
		{2, "net", pxeDevicePath()},
		{3, "http", uriDevicePath()},
	}
	for _, e := range entries {
		err := m.Set(Variable{
			GUID:       EFIGlobalVariable,
			Name:       fmt.Sprintf("Boot%04X", e.id),
			Attributes: AttrBootVariable,
			Data:       encodeTestLoadOption(e.desc, e.path),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := m.SetBootOrder([]uint16{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerBootOverride(t *testing.T) {
	m := newTestManager(t)
	setupBootEntries(t, m)

	// Once → BootNext points at the PXE entry.
	if err := m.SetBootSourceOverride(TargetPxe, true); err != nil {
		t.Fatalf("SetBootSourceOverride(Pxe, once) error: %v", err)
	}
	next, err := m.BootNext()
	if err != nil || next == nil || *next != 2 {
		t.Fatalf("BootNext = %v, %v; want 2", next, err)
	}
	target, enabled, err := m.BootSourceOverride()
	if err != nil || target != TargetPxe || enabled != "Once" {
		t.Errorf("BootSourceOverride() = %q, %q, %v", target, enabled, err)
	}

	// Continuous → BootOrder reordered with HTTP entry first.
	if err := m.SetBootSourceOverride(TargetUefiHttp, false); err != nil {
		t.Fatalf("SetBootSourceOverride(UefiHttp, continuous) error: %v", err)
	}
	order, err := m.BootOrder()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != 3 || order[1] != 1 || order[2] != 2 {
		t.Errorf("BootOrder = %v, want [3 1 2]", order)
	}

	// Clear removes BootNext.
	if err := m.ClearBootSourceOverride(); err != nil {
		t.Fatal(err)
	}
	if next, _ := m.BootNext(); next != nil {
		t.Errorf("BootNext still set after clear: %d", *next)
	}

	// No matching entry → ErrNoMatchingEntry.
	if err := m.SetBootSourceOverride(TargetCd, true); err == nil {
		t.Error("SetBootSourceOverride(Cd) succeeded with no CD entry")
	}
}

func TestUBootBlobCompatibility(t *testing.T) {
	// Hand-build a blob exactly as U-Boot's efi_var_collect() lays it out
	// and confirm Decode reads it: one variable, name "X" (UTF-16 "X\0"),
	// 2 data bytes.
	name := utf16.Encode([]rune("X"))
	entry := make([]byte, 0, 64)
	entry = binary.LittleEndian.AppendUint32(entry, 2)               // data length
	entry = binary.LittleEndian.AppendUint32(entry, AttrNonVolatile) // attr
	entry = binary.LittleEndian.AppendUint64(entry, 0)               // time
	entry = append(entry, EFIGlobalVariable[:]...)                   // guid
	for _, u := range name {
		entry = binary.LittleEndian.AppendUint16(entry, u)
	}
	entry = binary.LittleEndian.AppendUint16(entry, 0) // NUL
	entry = append(entry, 0xca, 0xfe)                  // data
	for len(entry)%8 != 0 {
		entry = append(entry, 0)
	}

	blob := make([]byte, 24, 24+len(entry))
	blob = append(blob, entry...)
	binary.LittleEndian.PutUint64(blob[8:16], Magic)
	binary.LittleEndian.PutUint32(blob[16:20], uint32(len(blob)))
	binary.LittleEndian.PutUint32(blob[20:24], crc32.ChecksumIEEE(blob[24:]))

	vars, err := Decode(blob)
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if len(vars) != 1 || vars[0].Name != "X" || !bytes.Equal(vars[0].Data, []byte{0xca, 0xfe}) {
		t.Fatalf("decoded %+v", vars)
	}
}
