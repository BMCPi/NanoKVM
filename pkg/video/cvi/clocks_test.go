package cvi

import "testing"

// The gate list is transcribed from drivers/clk/sophgo/clk-cv1800.c against a
// stock board's measured state. A duplicated entry, or one pointing at the
// wrong register, silently leaves a clock off -- and a clock left off in this
// subsystem does not report an error, it produces a pipeline that configures
// perfectly and then never delivers a frame.
func TestVIPClockGatesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	byBit := map[[2]uint64]string{}

	for _, g := range vipClockGates {
		if seen[g.name] {
			t.Errorf("%s listed twice", g.name)
		}
		seen[g.name] = true

		switch g.off {
		case clkEn2Off, clkEn3Off, clkEn4Off:
		default:
			t.Errorf("%s has offset %#x, not one of REG_CLK_EN_2/3/4", g.name, g.off)
		}

		if g.bit == 0 {
			t.Errorf("%s has no bit set", g.name)
		}

		key := [2]uint64{uint64(g.off), uint64(g.bit)}
		if other, dup := byBit[key]; dup {
			t.Errorf("%s and %s are the same gate (%#x bit %#x)", g.name, other, g.off, g.bit)
		}
		byBit[key] = g.name
	}
}

// The CSI receiver front end is the one gate whose absence was measured to
// stop packets assembling outright, so its identity is pinned rather than left
// to the list being edited correctly.
func TestCSIReceiverGateIsPresent(t *testing.T) {
	for _, g := range vipClockGates {
		if g.name != "clk_csi0_rx_vip" {
			continue
		}
		// REG_CLK_EN_3 bit 2, per CV1800_GATE(clk_csi0_rx_vip, ...).
		if g.off != clkEn3Off || g.bit != 1<<2 {
			t.Errorf("clk_csi0_rx_vip is %#x bit %#x, want %#x bit %#x",
				g.off, g.bit, clkEn3Off, 1<<2)
		}
		return
	}
	t.Error("clk_csi0_rx_vip is not in the gate list")
}
