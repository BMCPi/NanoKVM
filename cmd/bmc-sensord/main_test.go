package main

import "testing"

// The C original parses --bar with strtoull(..., 0), so an address may be
// written in hex, decimal or octal. Addresses are written in hex in practice
// and silently parsing "0x1f00000000" as zero would send the TA a null BAR.
func TestBarFlagParsesBasePrefixedAddresses(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
	}{
		{"0x1f00000000", 0x1f00000000},
		{"0X1F00000000", 0x1f00000000},
		{"133143986176", 0x1f00000000},
		{"0", 0},
	} {
		var h hexUint64
		if err := h.Set(tc.in); err != nil {
			t.Errorf("Set(%q): %v", tc.in, err)
			continue
		}
		if uint64(h) != tc.want {
			t.Errorf("Set(%q) = 0x%x, want 0x%x", tc.in, uint64(h), tc.want)
		}
	}
	for _, bad := range []string{"", "nonsense", "0xzz", "-1"} {
		var h hexUint64
		if err := h.Set(bad); err == nil {
			t.Errorf("Set(%q) succeeded, want an error", bad)
		}
	}
}

// The default is shown in usage output, where a decimal BAR is unreadable.
func TestBarFlagRendersHex(t *testing.T) {
	h := hexUint64(0x1f00000000)
	if got, want := h.String(), "0x1f00000000"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
