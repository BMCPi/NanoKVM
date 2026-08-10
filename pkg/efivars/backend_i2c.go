package efivars

// backend_i2c.go accesses a physical 24Cxx-style EEPROM as an I2C master
// through /dev/i2c-N using I2C_RDWR combined transactions: a 2-byte
// big-endian offset write, followed (for reads) by a repeated-start read.
//
// Only use this when the BMC is the sole master on the bus (host powered
// off or parked in a state where U-Boot is not accessing the store) —
// multi-master access to a shared EEPROM is not arbitrated here.

import (
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	i2cRdwrIoctl  = 0x0707 // I2C_RDWR
	i2cMsgRead    = 0x0001 // I2C_M_RD
	i2cReadChunk  = 1024   // per-transaction read size
	i2cWriteDelay = 10 * time.Millisecond
)

// i2cMsg mirrors the kernel's struct i2c_msg. buf is a real Go pointer so
// the referenced buffers stay visible to the garbage collector; it is only
// converted to a raw address at the syscall boundary.
type i2cMsg struct {
	addr  uint16
	flags uint16
	len   uint16
	_     uint16
	buf   *byte
}

// i2cRdwrData mirrors the kernel's struct i2c_rdwr_ioctl_data.
type i2cRdwrData struct {
	msgs  *i2cMsg
	nmsgs uint32
}

// I2CBackend is a raw-bus EEPROM store.
type I2CBackend struct {
	dev      string // /dev/i2c-N
	addr     uint16 // chip address (e.g. 0x50)
	pageSize int    // EEPROM write page size
	size     int    // store capacity in bytes
}

// NewI2CBackend returns a raw /dev/i2c-N backed store.
func NewI2CBackend(bus int, addr uint16, pageSize, size int) *I2CBackend {
	return &I2CBackend{
		dev:      fmt.Sprintf("/dev/i2c-%d", bus),
		addr:     addr,
		pageSize: pageSize,
		size:     size,
	}
}

func (b *I2CBackend) Size() int { return b.size }

func (b *I2CBackend) ReadAt(off int, p []byte) error {
	fd, err := os.OpenFile(b.dev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("efivars: open %s: %w", b.dev, err)
	}
	defer fd.Close()

	for len(p) > 0 {
		n := min(len(p), i2cReadChunk)
		if err := b.readChunk(fd, off, p[:n]); err != nil {
			return err
		}
		off += n
		p = p[n:]
	}
	return nil
}

func (b *I2CBackend) WriteAt(off int, p []byte) error {
	if b.size > 0 && off+len(p) > b.size {
		return fmt.Errorf("efivars: blob (%d bytes) exceeds store size (%d)", off+len(p), b.size)
	}
	fd, err := os.OpenFile(b.dev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("efivars: open %s: %w", b.dev, err)
	}
	defer fd.Close()

	for len(p) > 0 {
		// Never cross an EEPROM page boundary within one write cycle.
		n := min(len(p), b.pageSize-off%b.pageSize)
		if err := b.writeChunk(fd, off, p[:n]); err != nil {
			return err
		}
		// Wait out the EEPROM's internal write cycle (t_WR, typ. 5 ms).
		time.Sleep(i2cWriteDelay)
		off += n
		p = p[n:]
	}
	return nil
}

// readChunk issues [offset-write, repeated-start read] as one transaction.
func (b *I2CBackend) readChunk(fd *os.File, off int, p []byte) error {
	offBuf := []byte{byte(off >> 8 & 0xff), byte(off & 0xff)}
	msgs := []i2cMsg{
		{addr: b.addr, len: 2, buf: &offBuf[0]},
		{addr: b.addr, flags: i2cMsgRead, len: uint16(len(p)), buf: &p[0]}, //nolint:gosec // len capped at i2cReadChunk
	}
	return b.rdwr(fd, msgs)
}

// writeChunk issues a single [offset + data] write message.
func (b *I2CBackend) writeChunk(fd *os.File, off int, p []byte) error {
	buf := make([]byte, 2+len(p))
	buf[0] = byte(off >> 8 & 0xff)
	buf[1] = byte(off & 0xff)
	copy(buf[2:], p)
	msgs := []i2cMsg{
		{addr: b.addr, len: uint16(len(buf)), buf: &buf[0]}, //nolint:gosec // len capped at pageSize+2
	}
	return b.rdwr(fd, msgs)
}

func (b *I2CBackend) rdwr(fd *os.File, msgs []i2cMsg) error {
	data := i2cRdwrData{
		msgs:  &msgs[0],
		nmsgs: uint32(len(msgs)), //nolint:gosec // at most 2 messages
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd.Fd(),
		i2cRdwrIoctl, uintptr(unsafe.Pointer(&data))) // #nosec G103 -- kernel ABI requires the raw pointer
	runtime.KeepAlive(msgs)
	if errno != 0 {
		return fmt.Errorf("efivars: i2c transfer at %s addr %#x: %w", b.dev, b.addr, errno)
	}
	return nil
}
