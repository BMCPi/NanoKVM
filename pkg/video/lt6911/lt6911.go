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

	// Input timing and lock status.
	//
	// These are the UXC-style registers, not the ones Sipeed's software uses
	// for what it calls hdmi_version 0. Determined by dumping every bank with
	// a 1920x1080 source attached: bank 0xD2 reads back all zeroes, while bank
	// 0x86 holds a stable 0x03C0/0x0438 pair and a 0x55 at 0xA3. So the part
	// fitted to this board answers on 0x86 whatever its marking says, and
	// reading 0xD2 would report "no signal" forever.
	bankTiming = 0x86

	regLock   = 0xA3 // 0x55 signal stable, 0x88 signal lost
	lockValue = 0x55
	lostValue = 0x88

	regVActive = 0x8B // 2 bytes, big-endian, lines
	regHActive = 0x8D // 2 bytes, big-endian, pixel pairs

	// 0x80/0x82 mirror HActive/VActive. Left unused: one source is enough,
	// and having a second would only raise the question of which to trust.
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
func (b *Bridge) Enable() error { return b.setEnabled(true) }

// Disable closes register access again.
func (b *Bridge) Disable() error { return b.setEnabled(false) }

func (b *Bridge) setEnabled(on bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

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

	// Stop the bridge's watchdog while its registers are open.
	//
	// Sipeed's driver does this for the C variant and it is not cosmetic: a
	// running watchdog resets the bridge's firmware periodically, which
	// re-acquires HDMI lock each time and can leave the CSI transmitter
	// never reaching a steady state -- exactly the "HDMI locked, lanes idle"
	// shape seen on this board.
	return b.write(regWatchdog, 0x00)
}

// Signal describes what the bridge currently sees on its HDMI input.
//
// Locked false means no usable input -- unplugged, powered off, or a mode the
// bridge cannot measure. Width and Height are only meaningful when it is true.
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

	if err := b.selectBank(bankTiming); err != nil {
		return Signal{}, err
	}

	// Lock first. The timing registers hold their last measurement after the
	// source goes away, so reading them alone would keep reporting a
	// resolution for an unplugged cable.
	lock, err := b.read(regLock, 1)
	if err != nil {
		return Signal{}, err
	}
	if lock[0] != lockValue {
		return Signal{}, nil
	}

	v, err := b.read(regVActive, 2)
	if err != nil {
		return Signal{}, err
	}
	h, err := b.read(regHActive, 2)
	if err != nil {
		return Signal{}, err
	}

	height := int(v[0])<<8 | int(v[1])
	// Horizontal active is counted in pixel pairs.
	width := (int(h[0])<<8 | int(h[1])) * 2

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

	if err := b.selectBank(bankTiming); err != nil {
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
