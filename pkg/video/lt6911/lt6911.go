// Package lt6911 talks to the Lontium LT6911C HDMI-to-CSI-2 bridge that turns
// the host's HDMI output into the MIPI stream the SG2002's VI block captures.
//
// The bridge is the only part of the capture path that knows whether a cable
// is plugged in and what the host is actually sending, so this is what backs
// the "no signal" state and the resolution the pipeline has to be configured
// for. It runs its own firmware from an SPI flash; nothing here uploads code,
// it only reads status and drives the EDID the host sees.
//
// # Protocol
//
// Everything is banked. Register 0xFF selects a bank, and every other register
// address is interpreted within the bank last selected -- so a read is always
// at least two transactions and the bank has to be re-selected after anything
// that might have changed it. There is no register that reports the current
// bank, so this package never assumes one.
//
// Register numbers and the enable sequence follow Sipeed's own NanoKVM
// software (kvm_vision.cpp, nanokvm_update_edid.c), which is the only
// documentation of this part that exists -- Lontium publishes no public
// datasheet for the LT6911C.
package lt6911

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Addr is the bridge's 7-bit I2C address on this board.
const Addr = 0x2b

// DefaultDevice is the I2C bus the NanoKVM routes the bridge to. Reaching it
// at all needs the IIC4 pads muxed, which happens in U-Boot rather than
// through Linux pinctrl -- see the licheerv-nano mux-i2c4 patch in the BSP.
const DefaultDevice = "/dev/i2c-4"

// i2cSlave is I2C_SLAVE from linux/i2c-dev.h: bind this fd to a slave address
// so plain read/write go to that device.
const i2cSlave = 0x0703

// Register banks and the registers used within them.
const (
	regBank = 0xFF // bank select, valid in every bank

	bankEnable  = 0x80 // holds the I2C-access enable
	regEnable   = 0xEE // 0x01 opens register access, 0x00 closes it
	regWatchdog = 0x10 // 0x00 stops the bridge's internal watchdog

	// Lock status.
	//
	// This part answers as Sipeed's hdmi_version 1 (LT6911UXC): bank 0x86
	// register 0xA3 reads 0x55 while a source is stable and 0x88 once it goes
	// away. Confirmed by dumping every bank with a 1920x1080 source attached
	// -- bank 0xD2, which the hdmi_version 0 path uses, reads back all zeroes
	// here, so the C-variant registers would report "no signal" forever.
	bankLock = 0x86

	regLock   = 0xA3 // 0x55 signal stable, 0x88 signal lost
	lockValue = 0x55
	lostValue = 0x88

	// CSI transmit geometry.
	//
	// This is a different bank from the lock, and the distinction is the whole
	// point: what the bridge sees on HDMI and what it puts on the CSI lanes
	// are separate readings, and it is the latter the receiver has to be
	// configured for. Sipeed keeps them strictly apart -- for the UXC,
	// lt6911_get_hdmi_res() reads nothing but the lock byte above, while
	// lt6911_get_csi_res() reads these and it is *that* pair that becomes
	// vi_width/vi_height.
	//
	// Note there is no doubling here. The C-variant's HDMI Hactive is counted
	// in pixel pairs and Sipeed doubles it; these CSI registers are already in
	// pixels and Sipeed does not. Doubling them would configure VI for twice
	// the width the bridge is actually sending.
	bankCSI = 0x85

	regCSIVActive = 0xF0 // 2 bytes, big-endian, lines
	regCSIHActive = 0xEA // 2 bytes, big-endian, pixels
)

// Bridge is an open handle on the LT6911C.
//
// The mutex is not decoration: every read is a bank-select followed by a
// register access, and two goroutines interleaving those would each read from
// the other's bank.
type Bridge struct {
	mu sync.Mutex
	f  *os.File
}

// Open binds to the bridge on the given I2C bus. Pass "" for DefaultDevice.
func Open(device string) (*Bridge, error) {
	if device == "" {
		device = DefaultDevice
	}

	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("lt6911: open %s: %w", device, err)
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), i2cSlave, uintptr(Addr))
	if errno != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("lt6911: bind %s to 0x%02x: %w", device, Addr, errno)
	}

	return &Bridge{f: f}, nil
}

// Close releases the bus handle.
func (b *Bridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.f == nil {
		return nil
	}
	err := b.f.Close()
	b.f = nil
	return err
}

// write sends a register write. Callers hold b.mu.
func (b *Bridge) write(reg, val byte) error {
	if b.f == nil {
		return fmt.Errorf("lt6911: write to closed bridge")
	}
	if _, err := b.f.Write([]byte{reg, val}); err != nil {
		return fmt.Errorf("lt6911: write 0x%02x=0x%02x: %w", reg, val, err)
	}
	return nil
}

// read reads n bytes starting at reg. Callers hold b.mu.
//
// This is a write-then-read rather than a repeated-start combined transfer:
// the bridge latches the register address from the write and returns
// consecutive bytes on the following read, which is what Sipeed's driver does
// and what the part is known to tolerate.
func (b *Bridge) read(reg byte, n int) ([]byte, error) {
	if b.f == nil {
		return nil, fmt.Errorf("lt6911: read from closed bridge")
	}
	if _, err := b.f.Write([]byte{reg}); err != nil {
		return nil, fmt.Errorf("lt6911: select register 0x%02x: %w", reg, err)
	}
	buf := make([]byte, n)
	if _, err := b.f.Read(buf); err != nil {
		return nil, fmt.Errorf("lt6911: read %d bytes at 0x%02x: %w", n, reg, err)
	}
	return buf, nil
}

// selectBank points subsequent register accesses at bank. Callers hold b.mu.
func (b *Bridge) selectBank(bank byte) error {
	if err := b.write(regBank, bank); err != nil {
		return fmt.Errorf("lt6911: select bank 0x%02x: %w", bank, err)
	}
	return nil
}

// Enable opens register access. The bridge ignores most writes until this is
// done, so it has to precede any configuration.
//
// It must not be left open. Asserting this bit is how the host borrows the
// bridge's internal address space, and while it is asserted the bridge's own
// firmware is not driving the part -- so the CSI transmitter stops, while the
// timing registers keep returning their last latched measurement. The result
// reads as a healthy locked signal with idle lanes, which is a hard failure to
// attribute because every register you can ask still answers correctly.
//
// Sipeed's kvm_vision.cpp never holds it: every read is bracketed
// lt6911_enable() ... lt6911_disable(). Prefer Signal, which does the same.
func (b *Bridge) Enable() error { return b.setEnabled(true) }

// Disable closes register access again, handing the part back to its own
// firmware. See Enable for why this is not optional.
func (b *Bridge) Disable() error { return b.setEnabled(false) }

func (b *Bridge) setEnabled(on bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.setEnabledLocked(on)
}

// setEnabledLocked is setEnabled for callers already holding b.mu.
func (b *Bridge) setEnabledLocked(on bool) error {
	if err := b.selectBank(bankEnable); err != nil {
		return err
	}
	var v byte
	if on {
		v = 0x01
	}
	if err := b.write(regEnable, v); err != nil {
		return err
	}
	if !on {
		return nil
	}

	// Stop the bridge's watchdog for as long as its registers are open.
	//
	// Sipeed does this for the same window and it is not cosmetic: a running
	// watchdog resets the bridge's firmware periodically, and a reset landing
	// mid-read returns a torn measurement. It is restored implicitly when the
	// window closes and the part resumes running its own firmware.
	return b.write(regWatchdog, 0x00)
}

// Signal describes what the bridge is currently delivering.
//
// Locked false means no usable input -- unplugged, powered off, or a mode the
// bridge cannot measure. Width and Height are only meaningful when it is true.
//
// Width and Height are the geometry the bridge is transmitting on the CSI-2
// lanes, which is not necessarily the timing it is receiving over HDMI. That
// is the useful one: it is what the MIPI receiver and VI have to be configured
// for, and configuring them from the HDMI timing instead is how you get a
// pipeline that builds cleanly and never receives a frame.
type Signal struct {
	Locked bool
	Width  int
	Height int
}

func (s Signal) String() string {
	if !s.Locked {
		return "no signal"
	}
	return fmt.Sprintf("%dx%d", s.Width, s.Height)
}

// Signal reads the measured input timing.
//
// Horizontal active is reported in pixel pairs, hence the doubling; the vendor
// software does the same. A zero in either axis is what the bridge reports
// with nothing locked, so it is treated as absence of signal rather than as a
// read failure -- an unplugged cable is a normal state for a BMC, not an error.
func (b *Bridge) Signal() (Signal, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Open the register window for exactly this read and close it again on the
	// way out. Holding it open stops the bridge driving its CSI transmitter --
	// see Enable -- and a status poll that silently killed the video stream
	// would be a particularly unhelpful thing to own.
	if err := b.setEnabledLocked(true); err != nil {
		return Signal{}, err
	}
	defer func() { _ = b.setEnabledLocked(false) }()

	// Lock first, out of its own bank. The geometry registers hold their last
	// measurement after the source goes away, so reading them alone would keep
	// reporting a resolution for an unplugged cable.
	if err := b.selectBank(bankLock); err != nil {
		return Signal{}, err
	}
	lock, err := b.read(regLock, 1)
	if err != nil {
		return Signal{}, err
	}
	if lock[0] != lockValue {
		return Signal{}, nil
	}

	// Then the CSI transmit geometry, which lives in a different bank.
	if err := b.selectBank(bankCSI); err != nil {
		return Signal{}, err
	}
	v, err := b.read(regCSIVActive, 2)
	if err != nil {
		return Signal{}, err
	}
	h, err := b.read(regCSIHActive, 2)
	if err != nil {
		return Signal{}, err
	}

	height := int(v[0])<<8 | int(v[1])
	width := int(h[0])<<8 | int(h[1])

	if width == 0 || height == 0 {
		return Signal{}, nil
	}
	return Signal{Locked: true, Width: width, Height: height}, nil
}

// Lost reports whether the bridge is explicitly signalling that a source went
// away, as distinct from never having had one. Both read as not-locked from
// Signal; this separates them for diagnostics.
func (b *Bridge) Lost() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.selectBank(bankLock); err != nil {
		return false, err
	}
	v, err := b.read(regLock, 1)
	if err != nil {
		return false, err
	}
	return v[0] == lostValue, nil
}

// ReadBank dumps n registers from a bank. It exists for bring-up: this part
// has no public datasheet, so identifying a new register means reading around
// a known one and correlating with what the source is doing.
func (b *Bridge) ReadBank(bank, reg byte, n int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.selectBank(bank); err != nil {
		return nil, err
	}
	return b.read(reg, n)
}

// WriteBank sets one register in a bank. It is the write half of ReadBank and
// exists for the same reason: with no datasheet, confirming what a register
// does means writing it and watching what the rest of the system reports.
//
// It is deliberately not wrapped in a friendlier name. Everything this package
// does routinely has its own method; a caller reaching for a raw bank write is
// doing something whose effect is not yet understood, and the call site should
// say so.
func (b *Bridge) WriteBank(bank, reg, val byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.selectBank(bank); err != nil {
		return err
	}
	return b.write(reg, val)
}
