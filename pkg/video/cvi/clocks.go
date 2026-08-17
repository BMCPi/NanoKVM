package cvi

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The CSI receiver's clock gate.
//
// This one is a seam between two kernels rather than a bug in either. The
// vendor's out-of-tree media drivers claim their clocks by name, and between
// them they ask for every clock on the capture path except this one:
//
//	vi_core.c        clk_sys_0..3, clk_axi, clk_csi_be, clk_raw, clk_isp_top,
//	                 clk_csi_mac0..2
//	cif.c            clk_cam0, clk_cam1, clk_sys_2, clk_mipimpll, clk_disppll
//
// Nothing anywhere calls clk_get on clk_csi0_rx. On the vendor's own 5.10 BSP
// that costs nothing, because its clock driver leaves the VIP gates as the
// bootloader left them -- on. Upstream's clk-cv1800 does the opposite: any gate
// no driver has claimed is turned off by clk_disable_unused at late_initcall.
// So on a mainline kernel the receiver's front end loses its clock a moment
// after boot, before anything has had a chance to use it.
//
// The failure this produces is a quiet one, because the block that goes dark is
// not the block that reports status. Register access rides clk_cfg_reg_vip and
// keeps working, so every write lands and reads back; the MAC has its own gate
// and stays up; the D-PHY's analog side is not gated here at all, so the lane
// state machines still leave LP, still sync, and still sit in HS_HST with the
// clock lane locked. Everything observable says the link is up. But the RX
// front end never assembles a packet out of the bits, so the MAC ECC-checks
// nothing, VI is never handed a frame, and IntCnt stays at zero with no error
// anywhere to explain it.
//
// Poking a clock gate from userspace is not where this belongs. The honest fix
// is in the driver -- add the clock to vi_core.c's list and to the DT node --
// which means patching soph-media and rebuilding the image. This keeps the
// capture path working in the meantime, and is safe to leave in place after
// that: it is a single idempotent bit, clk_disable_unused has already run by
// the time any of this executes, and the framework's own gate operations are
// read-modify-write on their own bit, so nothing here is racing them.
const (
	clkgenBase = 0x03002000
	// Register offsets from drivers/clk/sophgo/clk-cv1800.h.
	clkEn2Off = 0x008
	clkEn3Off = 0x00C
	clkEn4Off = 0x010

	clkMapLen = 0x1000
)

// The gates to turn on, taken from a stock board with working capture.
//
// This list is not derived from what the datapath ought to need -- it is
// measured. A NanoKVM running Sipeed's own firmware, mid-stream, reports these
// enabled in /sys/kernel/debug/clk/clk_summary while ours reported every one of
// them at zero. Each is a gate that no soph-media driver calls clk_get on, so
// the vendor's 5.10 kernel leaves it as the bootloader left it and upstream's
// clk_disable_unused switches it off.
//
// clk_img_d_vip is deliberately absent: the stock board reads it as 0 too, so
// it belongs to a path this pipeline does not use, and enabling it would be
// guessing rather than matching.
var vipClockGates = []struct {
	name string
	off  uintptr
	bit  uint32
}{
	// The CSI receiver front end. Without it the D-PHY still locks and the
	// lanes still reach HS_HST, but nothing is ever assembled into a packet.
	{"clk_csi0_rx_vip", clkEn3Off, 1 << 2},

	// The VIP image path. Stock has all four of these on; the naming is
	// opaque and the vendor documents none of it, so they are enabled as a
	// set because that is how the working board has them.
	{"clk_vip_ip0", clkEn4Off, 1 << 9},
	{"clk_vip_ip1", clkEn4Off, 1 << 10},
	{"clk_vip_ip2", clkEn4Off, 1 << 11},
	{"clk_vip_ip3", clkEn4Off, 1 << 12},

	// The video-side image interface into the scaler.
	{"clk_img_v_vip", clkEn2Off, 1 << 22},
}

// setupCSIClock enables the capture path's clock gates if they are off.
//
// It must run before the receiver is configured. Enabling a gate afterwards is
// not enough on its own -- these blocks latch their configuration while
// clocked, so a link set up with the clock down stays deaf until it is set up
// again.
func setupCSIClock() error {
	f, err := os.OpenFile("/dev/mem", os.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		return fmt.Errorf("cvi: open /dev/mem for clock gate: %w", err)
	}
	defer f.Close()

	m, err := unix.Mmap(int(f.Fd()), clkgenBase, clkMapLen,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("cvi: mmap clock registers: %w", err)
	}
	defer unix.Munmap(m)

	for _, g := range vipClockGates {
		// Read-modify-write the single bit. The other gates in each word
		// belong to the clock framework, which is still tracking them.
		reg := (*uint32)(unsafe.Pointer(&m[g.off]))
		if *reg&g.bit == 0 {
			*reg |= g.bit
		}
	}
	return nil
}

// CSIClockState reports which of the capture path's gates are on, for bring-up
// diagnostics. The clock framework's own view of these (in /sys/kernel/debug/clk)
// stays at zero either way, because it is not the one that turned them on.
func CSIClockState() (map[string]bool, error) {
	f, err := os.OpenFile("/dev/mem", os.O_RDONLY|unix.O_SYNC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m, err := unix.Mmap(int(f.Fd()), clkgenBase, clkMapLen, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	defer unix.Munmap(m)

	out := make(map[string]bool, len(vipClockGates))
	for _, g := range vipClockGates {
		out[g.name] = *(*uint32)(unsafe.Pointer(&m[g.off]))&g.bit != 0
	}
	return out, nil
}
