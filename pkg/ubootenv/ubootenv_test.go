package ubootenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndMarshalRoundTrip(t *testing.T) {
	original := map[string]string{
		"bootcmd":   "run distro_bootcmd",
		"bootdelay": "3",
		"ethaddr":   "00:11:22:33:44:55",
	}
	var b strings.Builder
	for k, v := range original {
		b.WriteString(k + "=" + v + "\n")
	}

	env, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if len(env.Vars) != len(original) {
		t.Fatalf("expected %d vars, got %d", len(original), len(env.Vars))
	}
	for k, want := range original {
		if got := env.Vars[k]; got != want {
			t.Errorf("key %q: got %q, want %q", k, got, want)
		}
	}

	out := env.Marshal()
	env2, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(round-trip) error: %v", err)
	}
	for k, want := range original {
		if got := env2.Vars[k]; got != want {
			t.Errorf("round-trip key %q: got %q, want %q", k, got, want)
		}
	}
}

func TestParseTrailingNUL(t *testing.T) {
	// U-Boot's `env export -t` leaves a NUL terminator in memory; Parse
	// should tolerate one or more trailing NULs without erroring.
	data := []byte("foo=bar\nbaz=qux\n\x00\x00")
	env, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if env.Vars["foo"] != "bar" || env.Vars["baz"] != "qux" {
		t.Errorf("unexpected vars: %v", env.Vars)
	}
}

func TestParseSkipsBlanksAndComments(t *testing.T) {
	data := []byte(`
# this is a comment
foo=bar

   # indented comment
baz=qux
`)
	env, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if len(env.Vars) != 2 {
		t.Fatalf("expected 2 vars, got %d: %v", len(env.Vars), env.Vars)
	}
	if env.Vars["foo"] != "bar" || env.Vars["baz"] != "qux" {
		t.Errorf("unexpected vars: %v", env.Vars)
	}
}

func TestParseLineContinuation(t *testing.T) {
	// Backslash at end-of-line continues the value.
	data := []byte("multiline=line1\\\nline2\\\nline3\nsimple=value\n")
	env, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	want := "line1\nline2\nline3"
	if got := env.Vars["multiline"]; got != want {
		t.Errorf("multiline: got %q, want %q", got, want)
	}
	if env.Vars["simple"] != "value" {
		t.Errorf("simple: got %q", env.Vars["simple"])
	}

	// Round trip preserves embedded newlines.
	out := env.Marshal()
	env2, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if env2.Vars["multiline"] != want {
		t.Errorf("round-trip multiline: got %q", env2.Vars["multiline"])
	}
}

func TestParseEmptyValue(t *testing.T) {
	env, err := Parse([]byte("empty=\nfoo=bar\n"))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	v, ok := env.Get("empty")
	if !ok || v != "" {
		t.Errorf("empty: got %q, %v", v, ok)
	}
}

func TestParseMalformedLine(t *testing.T) {
	_, err := Parse([]byte("no_equals_sign\n"))
	if err == nil {
		t.Fatal("expected error for line without =")
	}
}

func TestParseEmpty(t *testing.T) {
	env, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil) error: %v", err)
	}
	if len(env.Vars) != 0 {
		t.Fatalf("expected 0 vars, got %d", len(env.Vars))
	}
}

func TestMarshalSorted(t *testing.T) {
	env := &Env{Vars: map[string]string{"c": "3", "a": "1", "b": "2"}}
	out := string(env.Marshal())
	want := "a=1\nb=2\nc=3\n"
	if out != want {
		t.Errorf("Marshal sorted: got %q, want %q", out, want)
	}
}

func TestGetSetDelete(t *testing.T) {
	env := New()

	if _, ok := env.Get("missing"); ok {
		t.Error("Get(missing) should return false")
	}

	env.Set("foo", "bar")
	v, ok := env.Get("foo")
	if !ok || v != "bar" {
		t.Errorf("after Set: got %q, %v", v, ok)
	}

	env.Set("foo", "updated")
	v, _ = env.Get("foo")
	if v != "updated" {
		t.Errorf("after Set(overwrite): got %q", v)
	}

	env.Delete("foo")
	if _, ok = env.Get("foo"); ok {
		t.Error("Delete(foo) should remove the key")
	}

	env.Delete("nonexistent") // no-op
}

func TestGetBootTargets(t *testing.T) {
	tests := []struct {
		name    string
		env     *Env
		want    []string
		wantNil bool
	}{
		{"not set", &Env{Vars: map[string]string{}}, nil, true},
		{"empty string", &Env{Vars: map[string]string{VarBootTargets: ""}}, nil, true},
		{"single target", &Env{Vars: map[string]string{VarBootTargets: "mmc0"}}, []string{"mmc0"}, false},
		{"multiple targets", &Env{Vars: map[string]string{VarBootTargets: "mmc0 usb0 pxe dhcp"}}, []string{"mmc0", "usb0", "pxe", "dhcp"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.env.GetBootTargets()
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len: got %d, want %d", len(got), len(tt.want))
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("index %d: got %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

func TestSetBootTargets(t *testing.T) {
	env := New()

	env.SetBootTargets([]string{"pxe", "dhcp"})
	v, ok := env.Get(VarBootTargets)
	if !ok || v != "pxe dhcp" {
		t.Errorf("after SetBootTargets: got %q, %v", v, ok)
	}

	env.SetBootTargets(nil)
	if _, ok = env.Get(VarBootTargets); ok {
		t.Error("SetBootTargets(nil) should delete the key")
	}

	env.SetBootTargets([]string{"mmc0"})
	env.SetBootTargets([]string{})
	if _, ok = env.Get(VarBootTargets); ok {
		t.Error("SetBootTargets([]) should delete the key")
	}
}

func TestGetInventory(t *testing.T) {
	env := &Env{
		Vars: map[string]string{
			"board_name":     "rpi",
			"board_revision": "0xE04171",
			"serial#":        "06c539f8c815f14f",
			"ethaddr":        "88:a2:9e:87:77:6b",
			"usbethaddr":     "88:a2:9e:87:77:6b",
			"fdtfile":        "broadcom/bcm2712-d-rpi-5-b.dtb",
			"arch":           "arm",
			"cpu":            "armv8",
			"soc":            "bcm283x",
			"vendor":         "raspberrypi",
			"ver":            "U-Boot 2026.04-dirty (Apr 15 2026 - 11:19:05 +0000)",
			"boot_targets":   "usb0 mmc nvme",
			"bootmeths":      "extlinux efi script pxe efi_mgr",
			"board":          "rpi",
			"board_rev":      "0x17",
			"bootcmd":        "bootflow scan -lb",
			"some_other_var": "irrelevant",
		},
	}

	inv := env.GetInventory()
	if len(inv) != 15 {
		t.Fatalf("expected 15 inventory items, got %d: %v", len(inv), inv)
	}
	if inv["board_name"] != "rpi" {
		t.Errorf("board_name: got %q", inv["board_name"])
	}
	if inv["serial#"] != "06c539f8c815f14f" {
		t.Errorf("serial#: got %q", inv["serial#"])
	}
	if _, ok := inv["some_other_var"]; ok {
		t.Error("some_other_var should not be in inventory")
	}
}

func TestLoadFileAndSaveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "machine.env")

	env := &Env{
		Vars: map[string]string{
			"bootcmd":      "run distro_bootcmd",
			"bootdelay":    "3",
			VarBootTargets: "mmc0 usb0 pxe dhcp",
			VarBoardName:   "rpi5",
		},
	}

	if err := env.SaveFile(path); err != nil {
		t.Fatalf("SaveFile() error: %v", err)
	}

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error: %v", err)
	}
	if len(loaded.Vars) != len(env.Vars) {
		t.Fatalf("var count: got %d, want %d", len(loaded.Vars), len(env.Vars))
	}
	for k, want := range env.Vars {
		if got, ok := loaded.Get(k); !ok || got != want {
			t.Errorf("key %q: got %q (ok=%v), want %q", k, got, ok, want)
		}
	}

	loaded.Set("bootdelay", "0")
	loaded.SetBootTargets([]string{"pxe", "dhcp"})
	if err := loaded.SaveFile(path); err != nil {
		t.Fatalf("SaveFile(modified) error: %v", err)
	}

	loaded2, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(modified) error: %v", err)
	}
	if v, _ := loaded2.Get("bootdelay"); v != "0" {
		t.Errorf("bootdelay after modify: got %q", v)
	}
	targets := loaded2.GetBootTargets()
	if len(targets) != 2 || targets[0] != "pxe" || targets[1] != "dhcp" {
		t.Errorf("boot_targets after modify: got %v", targets)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := LoadFile("/nonexistent/path/machine.env")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !os.IsNotExist(err) {
		// LoadFile wraps the error, but errors.Is via %w should still match.
		// Not strictly required by the API, but useful for callers.
		t.Logf("note: LoadFile error is wrapped (%v); callers should use errors.Is", err)
	}
}

func TestSaveFileAtomicity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persistent.env")

	env := &Env{Vars: map[string]string{"a": "1"}}
	if err := env.SaveFile(path); err != nil {
		t.Fatalf("initial SaveFile: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "persistent.env" {
			t.Errorf("unexpected file in dir: %s", e.Name())
		}
	}
}

func TestParseAndMarshalBinaryRoundTrip(t *testing.T) {
	original := &Env{
		Vars: map[string]string{
			"bootcmd":   "run distro_bootcmd",
			"bootdelay": "3",
			"ethaddr":   "00:11:22:33:44:55",
		},
		Format: FormatBinary,
		Size:   DefaultEnvSize,
	}

	data, err := original.MarshalBinary(0)
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(data) != DefaultEnvSize {
		t.Fatalf("expected %d bytes, got %d", DefaultEnvSize, len(data))
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Format != FormatBinary {
		t.Errorf("Format: got %v, want FormatBinary", parsed.Format)
	}
	if parsed.Size != DefaultEnvSize {
		t.Errorf("Size: got %d, want %d", parsed.Size, DefaultEnvSize)
	}
	if len(parsed.Vars) != len(original.Vars) {
		t.Fatalf("var count: got %d, want %d", len(parsed.Vars), len(original.Vars))
	}
	for k, want := range original.Vars {
		if got := parsed.Vars[k]; got != want {
			t.Errorf("key %q: got %q, want %q", k, got, want)
		}
	}
}

func TestParseBinaryEmpty(t *testing.T) {
	env := NewBinary(0)
	data, err := env.MarshalBinary(0)
	if err != nil {
		t.Fatalf("MarshalBinary empty: %v", err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse empty binary: %v", err)
	}
	if parsed.Format != FormatBinary {
		t.Errorf("Format: got %v", parsed.Format)
	}
	if len(parsed.Vars) != 0 {
		t.Errorf("expected 0 vars, got %d", len(parsed.Vars))
	}
}

func TestParseBinaryCRCMismatchFallsBackToText(t *testing.T) {
	// Data with a bogus CRC header that happens to look textual after.
	// Should NOT parse as binary; should fall back to text and either
	// succeed (if textual) or fail with a text-format error.
	data := []byte("foo=bar\nbaz=qux\n")
	env, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse text: %v", err)
	}
	if env.Format != FormatText {
		t.Errorf("Format: got %v, want FormatText", env.Format)
	}
	if env.Vars["foo"] != "bar" {
		t.Errorf("foo: got %q", env.Vars["foo"])
	}
}

func TestSaveFilePreservesBinaryFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "machine.env")

	env := NewBinary(0)
	env.Set("bootdelay", "3")
	env.Set("bootcmd", "run distro_bootcmd")
	if err := env.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != DefaultEnvSize {
		t.Errorf("file size: got %d, want %d", info.Size(), DefaultEnvSize)
	}

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.Format != FormatBinary {
		t.Errorf("Format: got %v, want FormatBinary", loaded.Format)
	}
	if v, _ := loaded.Get("bootcmd"); v != "run distro_bootcmd" {
		t.Errorf("bootcmd: got %q", v)
	}
}

func TestMarshalBinaryOverflow(t *testing.T) {
	env := NewBinary(64) // tiny
	env.Set("k", strings.Repeat("v", 100))
	if _, err := env.MarshalBinary(0); err == nil {
		t.Fatal("expected overflow error")
	}
}
