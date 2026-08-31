package bmcsensor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// DefaultPath is where Linux exposes the emulated EEPROM's contents.
//
// The name is assembled by the i2c core, not chosen by us: "0-1050" is bus 0
// (pinned by the i2c0 alias in the board DTS) and address 0x50 with
// I2C_ADDR_OFFSET_SLAVE (0x1000) added, which is how the core keeps a slave
// client from colliding with a master one at the same address.
// "slave-eeprom" is the binary attribute the i2c-slave-eeprom backend
// registers.
const DefaultPath = "/sys/bus/i2c/devices/0-1050/slave-eeprom"

// DefaultStaleAfter is how long a sequence number may sit unchanged before
// the sample stops counting as live.
//
// The EEPROM is RAM on this side: when the host powers off, its last record
// stays there and keeps parsing perfectly. Reporting that indefinitely would
// mean showing a plausible die temperature for a machine that is switched
// off, so a reading is only live while the sequence number keeps moving.
const DefaultStaleAfter = 90 * time.Second

// Reading is a parsed record plus what the BMC knows about it that the record
// itself cannot say.
type Reading struct {
	Record
	// At is when this sequence number was first observed here. It is BMC
	// wall time, unlike Record.UptimeSeconds which is the host's.
	At time.Time
	// Stale reports that the sequence number has not advanced within
	// StaleAfter — the host has stopped pushing, so the values are the
	// last ones it sent rather than current ones.
	Stale bool
}

// Reader reads samples from the EEPROM and tracks whether they are moving.
//
// The zero value is not usable; call NewReader. Safe for concurrent use: the
// Redfish handlers that consume it are served from many goroutines.
type Reader struct {
	path       string
	staleAfter time.Duration
	now        func() time.Time

	mu sync.Mutex
	// lastSeq and lastSeqAt are how staleness is measured. The record
	// carries no BMC-side timestamp, and its uptime field is the host's
	// clock, so "has this changed since we last looked" is the only
	// question this side can actually answer.
	lastSeq   uint32
	lastSeqAt time.Time
	haveSeq   bool
}

// NewReader reads from the default path.
func NewReader() *Reader { return NewReaderAt(DefaultPath, DefaultStaleAfter) }

// NewReaderAt reads from a named path, for tests and for a board whose bus
// numbering differs.
func NewReaderAt(path string, staleAfter time.Duration) *Reader {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	return &Reader{path: path, staleAfter: staleAfter, now: time.Now}
}

// Available reports whether the EEPROM attribute exists at all.
//
// It distinguishes "this kernel has no slave EEPROM" — the board DTS or the
// I2C_SLAVE_EEPROM config is missing — from "the host has not pushed yet",
// which read reports as ErrNoRecord. Callers use it to decide whether to
// offer the sensor at all rather than to report it as failed.
func (r *Reader) Available() bool {
	_, err := os.Stat(r.path)
	return err == nil
}

// Read returns the current sample.
//
// Errors are the record's (ErrNoRecord, ErrBadCRC, ...) or the filesystem's.
// A caller that only wants a temperature when there is a trustworthy one can
// treat every error the same way; the distinctions exist for logs.
func (r *Reader) Read() (Reading, error) {
	raw, err := r.readAt(RecordOffset, RecordSize)
	if err != nil {
		return Reading{}, err
	}
	rec, err := ParseRecord(raw)
	if err != nil {
		return Reading{}, err
	}

	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.haveSeq || rec.Seq != r.lastSeq {
		r.lastSeq, r.lastSeqAt, r.haveSeq = rec.Seq, now, true
	}
	return Reading{
		Record: rec,
		At:     r.lastSeqAt,
		Stale:  now.Sub(r.lastSeqAt) > r.staleAfter,
	}, nil
}

// readAt pulls one window out of the EEPROM attribute.
//
// ReadAt rather than reading the whole file: the attribute is the size of the
// emulated part (64 KiB), and the record is 32 bytes of it. sysfs binary
// attributes honour the offset, so there is no reason to haul the rest of it
// through a 128 MB BMC.
func (r *Reader) readAt(off int64, n int) ([]byte, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, fmt.Errorf("bmcsensor: open %s: %w", r.path, err)
	}
	defer f.Close()

	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil {
		// A short read means the attribute is smaller than the offset the
		// pTA writes at, which is a configuration mismatch (an 8-bit part
		// emulated where a 16-bit one is needed) rather than a bad sample.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("bmcsensor: %s is too small for a record at 0x%x", r.path, off)
		}
		return nil, fmt.Errorf("bmcsensor: read %s at 0x%x: %w", r.path, off, err)
	}
	return buf, nil
}
