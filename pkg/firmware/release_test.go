package firmware

import "testing"

func TestCompareUBootVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// User's literal rule: v2026.07 > v2026.07-rc1.
		{"v2026.07", "v2026.07-rc1", 1},
		{"v2026.07-rc1", "v2026.07", -1},
		// Equal versions.
		{"v2026.07", "v2026.07", 0},
		{"v2026.07-rc1", "v2026.07-rc1", 0},
		// Numeric ordering.
		{"v2026.07", "v2026.06", 1},
		{"v2026.06", "v2026.07", -1},
		{"v2027.01", "v2026.12", 1},
		// rc ordering.
		{"v2026.07-rc2", "v2026.07-rc1", 1},
		{"v2026.07-rc1", "v2026.07-rc2", -1},
		// Tag prefix tolerance.
		{"u-boot-v2026.07", "u-boot-v2026.07-rc1", 1},
		// "v" prefix optional.
		{"2026.07", "v2026.07", 0},
	}
	for _, tc := range cases {
		got := CompareUBootVersions(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("CompareUBootVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseUBootVer(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"U-Boot 2026.07-rc1 (Aug 28 2025 - 12:34:56 +0000)", "v2026.07-rc1"},
		{"U-Boot v2026.07 (...)", "v2026.07"},
		{"", ""},
		{"no version here", ""},
	}
	for _, tc := range cases {
		got := parseUBootVer(tc.in)
		if got != tc.want {
			t.Errorf("parseUBootVer(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
