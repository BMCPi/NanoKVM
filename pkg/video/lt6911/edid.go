package lt6911

import (
	"fmt"
	"time"
)

// EDID storage.
//
// The bridge serves an EDID to the host from an SPI flash it owns, and the
// host negotiates its output mode against it. A blank EDID leaves the host
// guessing: it will usually still send something, but nothing about the mode
// is under our control.
//
// The whole flash interface lives in the enable's bank behind register 0x5A,
// which is a command register rather than a data one -- 0x81 erases, 0x90
// writes, 0x84/0x88/0xA0/0x20 are chip-select and read/write-enable states.
// The sequences below are transcribed step for step from
// lt6911uxc_edid_write()/lt6911uxc_edid_read() in Sipeed's
// tools/nanokvm_update_edid/nanokvm_update_edid.c. Nothing here is derived:
// the ordering of those writes is the protocol.
const (
	edidSize  = 256
	edidChunk = 32 // LT6911UXC_WR_SIZE

	regFlashCmd  = 0x5A // command: 0x81 erase, 0x90 write, 0xA0 read
	regFlashLen  = 0x5E
	regFlashCtl  = 0x58
	regFlashData = 0x59
	regFlashAddH = 0x5B
	regFlashAddM = 0x5C
	regFlashAddL = 0x5D
	regFlashRead = 0x5F

	// bankGate holds the flash gate the vendor checks before erasing or
	// writing. It reads 0xEE when the interface is available; anything else
	// means the part is not ready and the sequence is abandoned.
	bankGate     = 0x81
	regFlashGate = 0x08
	flashGateOK  = 0xEE
	flashGateAck = 0xAE
)

// writeBytes sends a register address followed by n data bytes in one
// transfer, which is the vendor's i2c_write_bytes. Callers hold b.mu.
func (b *Bridge) writeBytes(reg byte, data []byte) error {
	if b.f == nil {
		return fmt.Errorf("lt6911: write to closed bridge")
	}
	buf := make([]byte, 0, len(data)+1)
	buf = append(buf, reg)
	buf = append(buf, data...)
	if _, err := b.f.Write(buf); err != nil {
		return fmt.Errorf("lt6911: write %d bytes at 0x%02x: %w", len(data), reg, err)
	}
	return nil
}

// ValidEDID reports whether b is a structurally sound EDID: the fixed header,
// then each 128-byte block summing to zero modulo 256. This is the same check
// the vendor's tool makes before it will program anything.
func ValidEDID(e []byte) bool {
	if len(e) != edidSize {
		return false
	}
	if e[0] != 0x00 || e[7] != 0x00 {
		return false
	}
	for i := 1; i < 7; i++ {
		if e[i] != 0xFF {
			return false
		}
	}
	var s0, s1 byte
	for i := 0; i < 128; i++ {
		s0 += e[i]
	}
	for i := 128; i < 256; i++ {
		s1 += e[i]
	}
	return s0 == 0 && s1 == 0
}

// ReadEDID returns the EDID currently stored in the bridge's flash.
func (b *Bridge) ReadEDID() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readEDIDLocked()
}

func (b *Bridge) readEDIDLocked() ([]byte, error) {
	if err := b.setEnabledLocked(true); err != nil {
		return nil, err
	}
	defer func() { _ = b.setEnabledLocked(false) }()

	if err := b.selectBank(bankEnable); err != nil {
		return nil, err
	}
	if err := b.write(regFlashCmd, 0x84); err != nil {
		return nil, err
	}
	if err := b.write(regFlashCmd, 0x80); err != nil {
		return nil, err
	}

	out := make([]byte, 0, edidSize)
	for i := 0; i < edidSize/edidChunk; i++ {
		for _, w := range []struct{ reg, val byte }{
			{regFlashLen, 0x5F},
			{regFlashCmd, 0xA0},
			{regFlashCmd, 0x80},
			{regFlashAddH, 0x01},
			{regFlashAddM, 0x80},
			{regFlashAddL, byte(edidChunk * i)},
			{regFlashCmd, 0x90},
			{regFlashCmd, 0x80},
			{regFlashCtl, 0x21},
		} {
			if err := b.write(w.reg, w.val); err != nil {
				return nil, err
			}
		}
		chunk, err := b.read(regFlashRead, edidChunk)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// checkGate reads the vendor's flash-ready gate and acknowledges it. A gate
// that does not read 0xEE means the flash interface is not in a state where
// erasing or writing is safe, and the caller must not proceed.
func (b *Bridge) checkGate(stage string) error {
	if err := b.selectBank(bankGate); err != nil {
		return err
	}
	v, err := b.read(regFlashGate, 1)
	if err != nil {
		return fmt.Errorf("lt6911: %s: read flash gate: %w", stage, err)
	}
	if v[0] != flashGateOK {
		return fmt.Errorf("lt6911: %s: flash gate reads 0x%02x, want 0x%02x", stage, v[0], flashGateOK)
	}
	if err := b.write(regFlashGate, flashGateAck); err != nil {
		return err
	}
	return b.write(regFlashGate, flashGateOK)
}

// EnsureEDID programs the bridge's EDID storage if what is there is not a
// valid EDID, and reports whether it wrote.
//
// It reads first and leaves a good EDID alone. That is not just politeness:
// this is a flash erase/write cycle, and doing it unconditionally on every
// start would spend the part's write endurance to no purpose.
func (b *Bridge) EnsureEDID() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	current, err := b.readEDIDLocked()
	if err != nil {
		return false, err
	}
	if ValidEDID(current) {
		return false, nil
	}

	// Never put something malformed into the flash, even from our own
	// constant -- a bad EDID is worse than none, because the host will
	// believe it.
	payload := defaultEDID[:]
	if !ValidEDID(payload) {
		return false, fmt.Errorf("lt6911: built-in EDID is malformed, refusing to write")
	}

	if err := b.writeEDIDLocked(payload); err != nil {
		return false, err
	}

	// Read back rather than trusting the write. A flash write that reports
	// success and stores nothing is exactly the failure this guards.
	after, err := b.readEDIDLocked()
	if err != nil {
		return true, fmt.Errorf("lt6911: edid written but read-back failed: %w", err)
	}
	if !ValidEDID(after) {
		return true, fmt.Errorf("lt6911: edid write did not take: read back invalid")
	}
	return true, nil
}

func (b *Bridge) writeEDIDLocked(edid []byte) error {
	if err := b.setEnabledLocked(true); err != nil {
		return err
	}
	defer func() { _ = b.setEnabledLocked(false) }()

	// --- erase ---
	if err := b.selectBank(bankEnable); err != nil {
		return err
	}
	for _, w := range []struct{ reg, val byte }{
		{regFlashLen, 0xDF},
		{regFlashCtl, 0x00},
		{regFlashData, 0x51},
		{regFlashCmd, 0x10},
		{regFlashCmd, 0x00},
		{regFlashCtl, 0x21},
	} {
		if err := b.write(w.reg, w.val); err != nil {
			return err
		}
	}
	if err := b.setEnabledLocked(true); err != nil {
		return err
	}
	if err := b.selectBank(bankEnable); err != nil {
		return err
	}
	for _, w := range []struct{ reg, val byte }{
		{regFlashCmd, 0x80},
		{regFlashCmd, 0x84},
		{regFlashCmd, 0x80},
		{regFlashAddH, 0x01},
		{regFlashAddM, 0x80},
		{regFlashAddL, 0x00},
		{regFlashCmd, 0x81}, // erase
		{regFlashCmd, 0x80},
	} {
		if err := b.write(w.reg, w.val); err != nil {
			return err
		}
	}
	// The erase is not acknowledged; the vendor simply waits it out.
	time.Sleep(500 * time.Millisecond)

	if err := b.checkGate("post-erase"); err != nil {
		return err
	}

	// --- write ---
	if err := b.setEnabledLocked(true); err != nil {
		return err
	}
	if err := b.selectBank(bankEnable); err != nil {
		return err
	}
	for _, v := range []byte{0x84, 0x80, 0x84, 0x80} {
		if err := b.write(regFlashCmd, v); err != nil {
			return err
		}
	}

	// One pass beyond the data: the vendor's wr_count is size/chunk + 1, and
	// the extra pass clears the version-string page at 0x81xx.
	passes := len(edid)/edidChunk + 1
	blank := make([]byte, edidChunk)

	for i := 0; i < passes; i++ {
		last := i == passes-1
		for _, w := range []struct{ reg, val byte }{
			{regFlashLen, 0xDF},
			{regFlashCmd, 0x20},
			{regFlashCmd, 0x00},
			{regFlashCtl, 0x21},
		} {
			if err := b.write(w.reg, w.val); err != nil {
				return err
			}
		}

		data := blank
		if !last {
			data = edid[edidChunk*i : edidChunk*(i+1)]
		}
		if err := b.writeBytes(regFlashData, data); err != nil {
			return err
		}

		if err := b.write(regFlashAddH, 0x01); err != nil {
			return err
		}
		if last {
			if err := b.write(regFlashAddM, 0x81); err != nil {
				return err
			}
			if err := b.write(regFlashAddL, 0x00); err != nil {
				return err
			}
		} else {
			if err := b.write(regFlashAddM, 0x80); err != nil {
				return err
			}
			// edid is always edidSize (256) bytes here -- EnsureEDID checks
			// ValidEDID(payload), which requires len(e) == edidSize, before
			// ever calling this -- and this branch only runs while !last,
			// i.e. i < len(edid)/edidChunk = 8. So edidChunk*i tops out at
			// 224, always within a byte.
			//nolint:gosec // bounded by edidSize/edidChunk; see comment above
			if err := b.write(regFlashAddL, byte(edidChunk*i)); err != nil {
				return err
			}
		}

		tail := byte(0x84)
		if last {
			tail = 0x88
		}
		for _, w := range []struct{ reg, val byte }{
			{regFlashLen, 0xC0},
			{regFlashCmd, 0x90}, // write
			{regFlashCmd, 0x80},
			{regFlashCmd, tail},
			{regFlashCmd, 0x80},
		} {
			if err := b.write(w.reg, w.val); err != nil {
				return err
			}
		}
	}

	return b.checkGate("post-write")
}
