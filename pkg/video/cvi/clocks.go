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
	// REG_CLK_EN_3, from drivers/clk/sophgo/clk-cv1800.h.
	clkEn3Off = 0x00C
	// clk_csi0_rx_vip is bit 2 of REG_CLK_EN_3. Only link 0 is enabled:
	// this board's bridge is on csi_mac0, and clk_csi1_rx would be power
	// spent on a receiver nothing is wired to.
	clkCsi0RxBit = 1 << 2

	clkMapLen = 0x1000
)

// setupCSIClock enables the CSI receiver clock gate if it is off.
//
// It must run before the receiver is configured. Enabling the gate afterwards
// is not enough on its own -- the front end latches its configuration while
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

	reg := (*uint32)(unsafe.Pointer(&m[clkEn3Off]))
	if *reg&clkCsi0RxBit == 0 {
		// Read-modify-write the single bit. The other gates in this word
		// belong to the clock framework, which is still tracking them.
		*reg |= clkCsi0RxBit
	}
	return nil
}

// CSIClockEnabled reports whether the CSI receiver clock gate is on, for
// bring-up diagnostics. The clock framework's own view of this gate (in
// /sys/kernel/debug/clk) stays at zero either way, because it is not the one
// that turned it on.
func CSIClockEnabled() (bool, error) {
	f, err := os.OpenFile("/dev/mem", os.O_RDONLY|unix.O_SYNC, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()

	m, err := unix.Mmap(int(f.Fd()), clkgenBase, clkMapLen, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return false, err
	}
	defer unix.Munmap(m)

	return *(*uint32)(unsafe.Pointer(&m[clkEn3Off]))&clkCsi0RxBit != 0, nil
}
