package lt6911

import "testing"

// The built-in EDID is written to flash, so a malformed constant would be
// programmed into hardware and believed by the host. Check it here rather than
// discovering it on a board.
func TestDefaultEDIDIsValid(t *testing.T) {
	if !ValidEDID(defaultEDID[:]) {
		t.Fatal("built-in EDID is not a valid EDID")
	}
}

func TestDefaultEDIDDescribes1080p(t *testing.T) {
	// First detailed timing descriptor starts at byte 54. Pixel clock is a
	// little-endian count of 10kHz units; 1080p60 is 148.50MHz.
	clk := (int(defaultEDID[55])<<8 | int(defaultEDID[54])) * 10
	if clk != 148500 {
		t.Errorf("detailed timing pixel clock = %d kHz, want 148500", clk)
	}

	// Active pixels are split across a low byte and the high nibble of the
	// byte shared with the matching blanking value: horizontal at 56 with its
	// high nibble in 58, vertical at 59 with its high nibble in 61.
	w := int(defaultEDID[56]) | int(defaultEDID[58]&0xF0)<<4
	h := int(defaultEDID[59]) | int(defaultEDID[61]&0xF0)<<4
	if w != 1920 || h != 1080 {
		t.Errorf("detailed timing active = %dx%d, want 1920x1080", w, h)
	}
}

func TestValidEDIDRejectsBadInput(t *testing.T) {
	blank := make([]byte, edidSize)
	for i := range blank {
		blank[i] = 0xFF
	}

	cases := []struct {
		name string
		in   []byte
	}{
		{"nil", nil},
		{"short", make([]byte, 128)},
		{"erased flash", blank},
		{"zeroed", make([]byte, edidSize)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ValidEDID(tc.in) {
				t.Error("accepted invalid EDID")
			}
		})
	}

	t.Run("bad checksum", func(t *testing.T) {
		bad := defaultEDID
		bad[100]++ // break block 0 without touching the header
		if ValidEDID(bad[:]) {
			t.Error("accepted EDID with a bad block-0 checksum")
		}
	})

	t.Run("bad header", func(t *testing.T) {
		bad := defaultEDID
		bad[3] = 0x00
		if ValidEDID(bad[:]) {
			t.Error("accepted EDID with a corrupt header")
		}
	})
}
