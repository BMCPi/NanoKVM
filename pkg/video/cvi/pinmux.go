package cvi

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// MIPI RX pad muxing.
//
// The ten pads carrying the five MIPI RX differential pairs are shared with
// other functions, and U-Boot leaves several of them pointed somewhere else.
// For this board (build/boards/sg200x/sg2002_licheervnano_sd/u-boot/
// cvi_board_init.c) it hands RX4N/RX4P/RX3N/RX3P to SPI1 and, worse, RX0N to
// CAM_MCLK1:
//
//	mmio_write_32(0x0300118C, 0x5); // RX0N CAM_MCLK1 for beta
//	mmio_write_32(0x0300116C, 0x7); // spi1 clk   GPIOC2 MIPI_RX4N
//	mmio_write_32(0x03001170, 0x7); // spi1 cs    GPIOC3 MIPI_RX4P
//	mmio_write_32(0x03001174, 0x7); // spi1 miso  GPIOC4 MIPI_RX3N
//	mmio_write_32(0x03001178, 0x7); // spi1 mosi  GPIOC5 MIPI_RX3P
//
// Nothing in the media stack undoes that: the cif driver muxes lanes *within*
// the D-PHY but never touches the pad function, and combo_dev_attr_s has no
// field for it. The vendor does it from userspace, at the top of lt6911_probe()
// before the first I2C access (middleware/v2/component/isp/sensor/cv182x/
// lontium_lt6911/lt6911_sensor_ctl.c:191), as ten devmem writes of function 3.
// That code lives in libsns_full, which this project does not use, so the step
// has to be reproduced here.
//
// Leaving it out does not present as a dead input. CAM_MCLK1 is a real clock
// driven onto the RX0N pad, so the receiver locks its clock lane to the SoC's
// own master clock and the lane state machines run -- while no CSI packet ever
// arrives, so every error counter stays at zero and VI takes no interrupt.
const (
	padMuxBase  = 0x03001000
	padMuxFirst = 0x0300116C // MIPI_RX4N
	padMuxLast  = 0x03001190 // MIPI_RX0P
	padMuxMipi  = 0x3        // the MIPI RX function for all ten pads

	padMuxMapOff = padMuxBase
	padMuxMapLen = 0x1000
)

// setupPinmux points the MIPI RX pads at their MIPI function.
//
// It is idempotent and cheap, and it has to run before the receiver is
// configured -- a lane whose pad is still wired to SPI1 will never carry data
// no matter how the D-PHY is muxed on top of it.
func setupPinmux() error {
	f, err := os.OpenFile("/dev/mem", os.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		return fmt.Errorf("cvi: open /dev/mem for pinmux: %w", err)
	}
	defer f.Close()

	m, err := unix.Mmap(int(f.Fd()), padMuxMapOff, padMuxMapLen,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("cvi: mmap pinmux registers: %w", err)
	}
	defer unix.Munmap(m)

	for addr := padMuxFirst; addr <= padMuxLast; addr += 4 {
		off := addr - padMuxMapOff
		reg := (*uint32)(unsafe.Pointer(&m[off]))
		if *reg != padMuxMipi {
			*reg = padMuxMipi
		}
	}
	return nil
}

// PadMuxState reports the ten pad function values, for bring-up diagnostics.
// Function 3 is MIPI; anything else means the pad is wired elsewhere and that
// lane cannot carry data.
func PadMuxState() ([]uint32, error) {
	f, err := os.OpenFile("/dev/mem", os.O_RDONLY|unix.O_SYNC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m, err := unix.Mmap(int(f.Fd()), padMuxMapOff, padMuxMapLen,
		unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	defer unix.Munmap(m)

	var out []uint32
	for addr := padMuxFirst; addr <= padMuxLast; addr += 4 {
		out = append(out, *(*uint32)(unsafe.Pointer(&m[addr-padMuxMapOff])))
	}
	return out, nil
}
