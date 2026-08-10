package eepromkeys

import (
	"strings"
	"testing"
)

func TestForPlatform_FiltersPi5Exclusions(t *testing.T) {
	pi5 := ForPlatform(PlatformRPi5)
	for _, k := range pi5 {
		if k.Name == "WAKE_ON_GPIO" {
			t.Errorf("WAKE_ON_GPIO leaked into Pi5 catalog; doc says it doesn't apply")
		}
		if k.Name == "USB_MSD_PWR_OFF_TIME" {
			t.Errorf("USB_MSD_PWR_OFF_TIME is Pi4-only; should not appear in Pi5 catalog")
		}
	}
	// Sanity: Pi5-only key should be present.
	if _, ok := findKey(pi5, "WAIT_FOR_POWER_BUTTON"); !ok {
		t.Error("expected WAIT_FOR_POWER_BUTTON in Pi5 catalog")
	}
}

func TestForPlatform_FiltersPi4Exclusions(t *testing.T) {
	pi4 := ForPlatform(PlatformRPi4)
	if _, ok := findKey(pi4, "WAIT_FOR_POWER_BUTTON"); ok {
		t.Error("WAIT_FOR_POWER_BUTTON is Pi5-only; should not appear in Pi4 catalog")
	}
	if _, ok := findKey(pi4, "WAKE_ON_GPIO"); !ok {
		t.Error("expected WAKE_ON_GPIO in Pi4 catalog (NotPlatforms excludes Pi5, not Pi4)")
	}
	if _, ok := findKey(pi4, "USB_MSD_PWR_OFF_TIME"); !ok {
		t.Error("expected USB_MSD_PWR_OFF_TIME in Pi4 catalog (OnlyPlatforms Pi4)")
	}
}

func findKey(ks []Key, name string) (Key, bool) {
	for _, k := range ks {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

func TestEqualValues_HexNormalisation(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0xf41", "0xF41", true},
		{"0xf41", "f41", true},
		{"3841", "0xf01", true}, // 0xf01 == 3841 decimal — but hex compares as hex
		{"0xff", "0x100", false},
	}
	for _, c := range cases {
		got := EqualValues(TypeHex, c.a, c.b)
		// "3841" parsed as hex = 0x3841, not 3841 dec; so the third case
		// should be false. Adjust expectation.
		if c.a == "3841" && c.b == "0xf01" {
			c.want = false
		}
		if got != c.want {
			t.Errorf("EqualValues(hex, %q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestEqualValues_IntegerAcceptsHex(t *testing.T) {
	// strconv.ParseInt with base 0 allows "0x..." → both sides should
	// compare equal as integers.
	if !EqualValues(TypeInt, "0x10", "16") {
		t.Error("0x10 == 16 (integer) expected true")
	}
}

func TestIsDefault_Pi5(t *testing.T) {
	if !IsDefault(PlatformRPi5, "BOOT_UART", "0") {
		t.Error("BOOT_UART=0 should be Pi5 default")
	}
	if IsDefault(PlatformRPi5, "BOOT_UART", "1") {
		t.Error("BOOT_UART=1 should NOT be default")
	}
	if !IsDefault(PlatformRPi5, "BOOT_ORDER", "0xF41") {
		t.Error("BOOT_ORDER=0xF41 should equal default 0xf41 (case-insensitive hex)")
	}
	if IsDefault(PlatformRPi5, "WAKE_ON_GPIO", "1") {
		t.Error("WAKE_ON_GPIO is Pi4-only; should not be considered a Pi5 default match")
	}
	if IsDefault(PlatformRPi5, "PSU_MAX_CURRENT", "5000") {
		t.Error("PSU_MAX_CURRENT default is empty; nothing should match")
	}
}

func TestFilterDefaultsFromBootconf_DropsAllSectionDefaults(t *testing.T) {
	in := strings.Join([]string{
		"[all]",
		"BOOT_UART=0",              // default — drop
		"BOOT_ORDER=0xf41",         // default — drop
		"POWER_OFF_ON_HALT=1",      // not default — keep
		"# comment line",           // keep
		"BOOT_WATCHDOG_TIMEOUT=10", // not default — keep
		"",                         // blank — keep
		"[gpio4=1]",                // header
		"BOOT_UART=0",              // conditional section — keep verbatim
		"BOOT_ORDER=0xf41",         // ditto
	}, "\n") + "\n"

	out := FilterDefaultsFromBootconf(PlatformRPi5, in)

	if strings.Contains(out, "BOOT_ORDER=0xf41") &&
		!strings.Contains(out, "[gpio4=1]\nBOOT_UART=0\nBOOT_ORDER=0xf41") {
		t.Error("[all] BOOT_ORDER default not dropped, OR conditional BOOT_ORDER wrongly dropped")
	}
	if strings.Contains(strings.SplitN(out, "[gpio4=1]", 2)[0], "BOOT_UART=0") {
		t.Error("[all] BOOT_UART=0 (default) should have been dropped")
	}
	if !strings.Contains(out, "POWER_OFF_ON_HALT=1") {
		t.Error("non-default key dropped unexpectedly")
	}
	if !strings.Contains(out, "BOOT_WATCHDOG_TIMEOUT=10") {
		t.Error("non-default key dropped unexpectedly")
	}
	if !strings.Contains(out, "# comment line") {
		t.Error("comment dropped unexpectedly")
	}
	if !strings.Contains(out, "[gpio4=1]") {
		t.Error("conditional section header dropped unexpectedly")
	}
}

func TestExtractNonAllSections(t *testing.T) {
	in := "[all]\nBOOT_UART=1\n[gpio4=1]\nBOOT_ORDER=0x1\n[HDMI=0]\nDISABLE_HDMI=1\n"
	got := ExtractNonAllSections(in)
	if !strings.Contains(got, "[gpio4=1]") || !strings.Contains(got, "[HDMI=0]") {
		t.Errorf("missing conditional sections in output:\n%s", got)
	}
	if strings.Contains(got, "[all]") || strings.Contains(got, "BOOT_UART=1") {
		t.Errorf("[all] content leaked into non-all extraction:\n%s", got)
	}
}

func TestSerializeAllSection_DropsDefaults(t *testing.T) {
	attrs := map[string]string{
		"BOOT_UART":             "0",     // default — drop
		"BOOT_ORDER":            "0xf41", // default — drop
		"POWER_OFF_ON_HALT":     "1",     // non-default — keep
		"BOOT_WATCHDOG_TIMEOUT": "30",
	}
	out := SerializeAllSection(PlatformRPi5, attrs, "")
	if !strings.HasPrefix(out, "[all]\n") {
		t.Errorf("expected [all] header; got:\n%s", out)
	}
	if strings.Contains(out, "BOOT_UART=") {
		t.Errorf("default key not dropped:\n%s", out)
	}
	if !strings.Contains(out, "POWER_OFF_ON_HALT=1") || !strings.Contains(out, "BOOT_WATCHDOG_TIMEOUT=30") {
		t.Errorf("non-default keys missing:\n%s", out)
	}
}

func TestSerializeAllSection_AppendsExistingNonAll(t *testing.T) {
	attrs := map[string]string{"POWER_OFF_ON_HALT": "1"}
	extras := "[gpio4=1]\nBOOT_ORDER=0x1\n"
	out := SerializeAllSection(PlatformRPi5, attrs, extras)
	if !strings.Contains(out, "[all]\nPOWER_OFF_ON_HALT=1") {
		t.Errorf("[all] block missing or wrong:\n%s", out)
	}
	if !strings.Contains(out, "[gpio4=1]\nBOOT_ORDER=0x1") {
		t.Errorf("existing non-[all] sections not appended:\n%s", out)
	}
}

func TestParseAllSection_IgnoresConditionalSections(t *testing.T) {
	in := "[all]\nBOOT_UART=1\nBOOT_ORDER=0x123\n[gpio4=1]\nBOOT_ORDER=0x456\n"
	got := ParseAllSection(in)
	if got["BOOT_UART"] != "1" {
		t.Errorf("BOOT_UART = %q; want 1", got["BOOT_UART"])
	}
	if got["BOOT_ORDER"] != "0x123" {
		t.Errorf("BOOT_ORDER = %q; want 0x123 (conditional section must not overwrite)", got["BOOT_ORDER"])
	}
}

// TestParseAllSection_CaseInsensitiveSection covers the regression that
// motivated normaliseSectionName: tooling that writes [All] or [ALL]
// must still be recognized as the universal section. Same for the
// implicit-no-header case (key=value before any [section]).
func TestParseAllSection_CaseInsensitiveSection(t *testing.T) {
	cases := map[string]string{
		"[All]":            "[All]\nBOOT_UART=1\n",
		"[ALL]":            "[ALL]\nBOOT_UART=1\n",
		"[ all ]":          "[ all ]\nBOOT_UART=1\n",
		"no header at all": "BOOT_UART=1\n",
	}
	for name, in := range cases {
		got := ParseAllSection(in)
		if got["BOOT_UART"] != "1" {
			t.Errorf("%s: BOOT_UART = %q; want 1 (full parse: %#v)", name, got["BOOT_UART"], got)
		}
	}
}

func TestListSections_OrderAndImplicitAll(t *testing.T) {
	in := "BOOT_UART=1\n[gpio4=1]\nFOO=BAR\n[HDMI=0]\nBAZ=1\n[all]\nQUX=1\n"
	got := ListSections(in)
	want := []string{"all", "gpio4=1", "hdmi=0"}
	if len(got) != len(want) {
		t.Fatalf("ListSections = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListSections[%d] = %q; want %q (full = %v)", i, got[i], want[i], got)
		}
	}
}

func TestListSections_Empty(t *testing.T) {
	if got := ListSections(""); len(got) != 0 {
		t.Errorf("empty input → %v; want []", got)
	}
}
