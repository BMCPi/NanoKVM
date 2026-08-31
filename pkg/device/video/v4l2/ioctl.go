// Package v4l2 implements the HDMI capture pipeline over the kernel's
// standard V4L2 API, against the soph_v4l2 media-controller front-end that
// now owns the vendor VI -> VPSS -> VENC drivers.
//
// Everything pkg/video/cvi used to do from userspace -- bring-up ordering, VB
// pools, the pull-model encoder feed, register pokes, the bridge polling --
// lives behind STREAMON now (see pkg/video/cvi in git history for the
// userspace-driven predecessor). What is left for this package is the standard
// V4L2 dance: set a format, map some buffers, stream, and listen for
// source-change events.
package v4l2

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl request encoding, asm-generic (riscv64 and every other arch this
// runs on): direction in bits 31..30, argument size in 29..16, type in
// 15..8, command in 7..0.
//
// Requests are computed from the Go struct sizes rather than transcribed,
// exactly as the cvi package did for the vendor ABI: a layout mistake then
// shows up as a wrong request number and an immediate ENOTTY, not as silent
// struct corruption. The layouts themselves are pinned by ioctl_test.go
// against sizes verified with the kernel's own headers.
const (
	iocWrite uintptr = 1
	iocRead  uintptr = 2
)

const vidiocType = 'V'

func ioc(dir, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | vidiocType<<8 | nr
}

// The one deliberately-literal size: struct v4l2_dv_timings is packed to
// 132 bytes in C, but its Go mirror carries a uint64 so Go pads it to 136.
// The kernel copies sizeof-C bytes; our buffer being 4 bytes longer is
// harmless in both directions.
const dvTimingsABISize = 132

func reqQuerycap() uintptr { return ioc(iocRead, 0, unsafe.Sizeof(capability{})) }
func reqGFmt() uintptr     { return ioc(iocRead|iocWrite, 4, unsafe.Sizeof(format{})) }
func reqSFmt() uintptr     { return ioc(iocRead|iocWrite, 5, unsafe.Sizeof(format{})) }
func reqReqbufs() uintptr {
	return ioc(iocRead|iocWrite, 8, unsafe.Sizeof(requestBuffers{}))
}
func reqQuerybuf() uintptr  { return ioc(iocRead|iocWrite, 9, unsafe.Sizeof(buffer{})) }
func reqQbuf() uintptr      { return ioc(iocRead|iocWrite, 15, unsafe.Sizeof(buffer{})) }
func reqDqbuf() uintptr     { return ioc(iocRead|iocWrite, 17, unsafe.Sizeof(buffer{})) }
func reqStreamon() uintptr  { return ioc(iocWrite, 18, 4) }
func reqStreamoff() uintptr { return ioc(iocWrite, 19, 4) }
func reqSParm() uintptr {
	return ioc(iocRead|iocWrite, 22, unsafe.Sizeof(streamParm{}))
}
func reqSCtrl() uintptr { return ioc(iocRead|iocWrite, 28, unsafe.Sizeof(control{})) }
func reqEnumInput() uintptr {
	return ioc(iocRead|iocWrite, 26, unsafe.Sizeof(input{}))
}

func reqSubscribeEvent() uintptr {
	return ioc(iocWrite, 90, unsafe.Sizeof(eventSubscription{}))
}
func reqDqevent() uintptr        { return ioc(iocRead, 89, unsafe.Sizeof(event{})) }
func reqQueryDvTimings() uintptr { return ioc(iocRead, 99, dvTimingsABISize) }
func reqGEdid() uintptr          { return ioc(iocRead|iocWrite, 40, unsafe.Sizeof(v4l2Edid{})) }
func reqSEdid() uintptr          { return ioc(iocRead|iocWrite, 41, unsafe.Sizeof(v4l2Edid{})) }

// Enum and flag constants, from the kernel's uapi headers.
const (
	bufTypeVideoCapture = 1
	memoryMMAP          = 1

	bufFlagKeyframe = 0x00000008

	pixFmtH264  = 'H' | '2'<<8 | '6'<<16 | '4'<<24
	pixFmtHEVC  = 'H' | 'E'<<8 | 'V'<<16 | 'C'<<24
	pixFmtMJPEG = 'M' | 'J'<<8 | 'P'<<16 | 'G'<<24

	cidCodecBase     = 0x00990900
	cidBitrate       = cidCodecBase + 207
	cidGopSize       = cidCodecBase + 203
	cidForceKeyframe = cidCodecBase + 229

	eventSourceChange   = 5
	srcChangeResolution = 1 << 0
)

// Struct mirrors of the V4L2 uapi, 64-bit little-endian layout. Sizes are
// pinned by ioctl_test.go: capability=104, format=208, streamparm=204,
// control=8, requestbuffers=20, buffer=88, eventSubscription=32, event=136,
// input=80.

type capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	_            [3]uint32
}

type pixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	ColorSpace   uint32
	Priv         uint32
	Flags        uint32
	YcbcrEnc     uint32
	Quantization uint32
	XferFunc     uint32
}

type format struct {
	Type uint32
	_    uint32 // the C union is 8-aligned (it holds pointer-bearing members)
	Pix  pixFormat
	_    [200 - unsafe.Sizeof(pixFormat{})]byte
}

type fract struct {
	Numerator   uint32
	Denominator uint32
}

type captureParm struct {
	Capability   uint32
	CaptureMode  uint32
	TimePerFrame fract
	ExtendedMode uint32
	ReadBuffers  uint32
	_            [4]uint32
}

type streamParm struct {
	Type    uint32
	Capture captureParm
	_       [200 - unsafe.Sizeof(captureParm{})]byte
}

type control struct {
	ID    uint32
	Value int32
}

type requestBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint8
	_            [3]uint8
}

type timeval struct {
	Sec  int64
	Usec int64
}

type timecode struct {
	Type    uint32
	Flags   uint32
	Frames  uint8
	Seconds uint8
	Minutes uint8
	Hours   uint8
	_       [4]uint8
}

type buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	_         uint32 // timestamp is 8-aligned
	Timestamp timeval
	Timecode  timecode
	Sequence  uint32
	Memory    uint32
	// M is the C union {offset; userptr; planes; fd}. For MMAP buffers the
	// mmap offset is the low 32 bits (little-endian).
	M         uint64
	Length    uint32
	Reserved2 uint32
	RequestFD int32
	_         uint32
}

type eventSubscription struct {
	Type  uint32
	ID    uint32
	Flags uint32
	_     [5]uint32
}

type timespec struct {
	Sec  int64
	Nsec int64
}

type event struct {
	Type      uint32
	_         uint32 // the C union holds an s64-bearing ctrl member
	U         [64]byte
	Pending   uint32
	Sequence  uint32
	Timestamp timespec
	ID        uint32
	_         [8]uint32
	_         uint32 // Go rounds the struct to 8; C is 136 exactly
}

// srcChanges extracts v4l2_event_src_change.changes from the event union.
func (e *event) srcChanges() uint32 {
	return uint32(e.U[0]) | uint32(e.U[1])<<8 | uint32(e.U[2])<<16 |
		uint32(e.U[3])<<24
}

// dvTimings is struct v4l2_dv_timings, which C declares packed -- a field
// mirror would inherit Go's alignment and drift, so it stays a byte array
// with accessors for the two fields this package reads. Layout: type at 0,
// then the bt union arm: width at 4, height at 8.
type dvTimings struct {
	raw [dvTimingsABISize]byte
}

func (t *dvTimings) width() uint32 {
	return uint32(t.raw[4]) | uint32(t.raw[5])<<8 | uint32(t.raw[6])<<16 |
		uint32(t.raw[7])<<24
}

func (t *dvTimings) height() uint32 {
	return uint32(t.raw[8]) | uint32(t.raw[9])<<8 | uint32(t.raw[10])<<16 |
		uint32(t.raw[11])<<24
}

// v4l2Edid is struct v4l2_edid; the core copies the pointed-to blocks
// between user and kernel space itself (array-argument handling).
type v4l2Edid struct {
	Pad        uint32
	StartBlock uint32
	Blocks     uint32
	_          [5]uint32
	Edid       *byte
}

type input struct {
	Index        uint32
	Name         [32]byte
	Type         uint32
	AudioSet     uint32
	Tuner        uint32
	Std          uint64
	Status       uint32
	Capabilities uint32
	_            [3]uint32
}

// vioctl issues one ioctl and maps failure to a wrapped errno.
func vioctl(fd int, req uintptr, arg unsafe.Pointer, what string) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return fmt.Errorf("v4l2: %s: %w", what, errno)
	}
	return nil
}
