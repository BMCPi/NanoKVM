package optee

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl request encoding, asm-generic (arm64, riscv64, amd64 alike):
// direction in bits 31..30, argument size in 29..16, type in 15..8, command
// in 7..0.
//
// Requests are computed from the Go struct sizes rather than transcribed, the
// same way pkg/video/v4l2 does it: a layout mistake then surfaces as a wrong
// request number and an immediate ENOTTY, rather than as silent struct
// corruption across the kernel boundary. ioctl_test.go pins both the sizes
// and the resulting request numbers against values read out of the kernel's
// own linux/tee.h.
const (
	// Every TEE ioctl is declared _IOR in linux/tee.h — including the ones
	// that are really writes, such as open-session and invoke. The direction
	// bits are part of the request number, so they are reproduced as the
	// kernel spells them rather than as they read; there is no second
	// direction to parameterise over.
	iocRead uintptr = 2

	teeIocMagic uintptr = 0xa4
)

func ioc(nr, size uintptr) uintptr {
	return iocRead<<30 | size<<16 | teeIocMagic<<8 | nr
}

func reqVersion() uintptr     { return ioc(0, unsafe.Sizeof(versionData{})) }
func reqOpenSession() uintptr { return ioc(2, unsafe.Sizeof(bufData{})) }
func reqInvoke() uintptr      { return ioc(3, unsafe.Sizeof(bufData{})) }
func reqCancel() uintptr      { return ioc(4, unsafe.Sizeof(cancelArg{})) }
func reqCloseSession() uintptr {
	return ioc(5, unsafe.Sizeof(closeSessionArg{}))
}

// TEE implementation ids (linux/tee.h).
const implIDOptee = 1

// Parameter attributes. Only the value types are here: this client passes no
// memory references, so it never has to allocate or register shared memory.
const (
	paramTypeNone        = 0
	paramTypeValueInput  = 1
	paramTypeValueOutput = 2
	paramTypeValueInOut  = 3
)

// Login classes (linux/tee.h). LoginPublic is what a session to a pseudo-TA
// uses; the others need a client UUID this package does not derive.
const LoginPublic = 0

// paramCount is how many parameters every request carries.
//
// Fixed at four to match libteec, which always sends TEEC_CONFIG_PAYLOAD_REF_COUNT
// parameters and pads the unused tail with TEE_IOCTL_PARAM_ATTR_TYPE_NONE. The
// kernel accepts a shorter array, but a TA that reads a fixed parameter block
// is entitled to see the same shape libteec would have sent it.
const paramCount = 4

// Struct mirrors of linux/tee.h, 64-bit little-endian layout. Sizes are
// pinned by ioctl_test.go: versionData=12, bufData=16, ioctlParam=32,
// openSessionArg=56, invokeArg=24, cancelArg=8, closeSessionArg=4.

type versionData struct {
	ImplID   uint32
	ImplCaps uint32
	GenCaps  uint32
}

// bufData is the variable-sized-buffer wrapper the open-session and invoke
// ioctls actually take; BufPtr addresses an openSessionBuf or invokeBuf.
type bufData struct {
	BufPtr uint64
	BufLen uint64
}

type ioctlParam struct {
	Attr uint64
	A    uint64
	B    uint64
	C    uint64
}

type openSessionArg struct {
	UUID      [16]byte
	ClntUUID  [16]byte
	ClntLogin uint32
	CancelID  uint32
	Session   uint32
	Ret       uint32
	RetOrigin uint32
	NumParams uint32
}

// openSessionBuf is openSessionArg with its flexible params[] tail
// materialised. The header is 56 bytes and ioctlParam is 8-aligned, so Go
// lays the array down at the same offset C does — asserted in the test rather
// than assumed.
type openSessionBuf struct {
	Arg    openSessionArg
	Params [paramCount]ioctlParam
}

type invokeArg struct {
	Func      uint32
	Session   uint32
	CancelID  uint32
	Ret       uint32
	RetOrigin uint32
	NumParams uint32
}

type invokeBuf struct {
	Arg    invokeArg
	Params [paramCount]ioctlParam
}

type cancelArg struct {
	CancelID uint32
	Session  uint32
}

type closeSessionArg struct {
	Session uint32
}

// teeIoctl issues one ioctl and maps failure to a wrapped errno.
func teeIoctl(fd int, req uintptr, arg unsafe.Pointer, what string) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return fmt.Errorf("optee: %s: %w", what, errno)
	}
	return nil
}
