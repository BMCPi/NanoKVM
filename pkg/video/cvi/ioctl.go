// Package cvi drives the CVITek multimedia pipeline on the Sophgo SG2002
// through its character-device ioctl interface.
//
// The vendor builds its whole VPU SDK into the kernel module
// (-DUSE_KERNEL_MODE in meta-sophgo/recipes-kernel/soph-media), so there is no
// userspace MPI library to link against and none is needed: the ioctl surface
// is the API. That also means the capture path never crosses into userspace.
// CVI_SYS_Bind wires VI -> VPSS -> VENC inside the kernel, and this package
// only pulls the finished bitstream out of the encoder -- hundreds of KB/s
// rather than the ~93 MB/s of raw frames a V4L2 mem2mem pipeline would have to
// copy on a board with one 1 GHz core.
//
// Nothing here uses cgo. The struct layouts in types_linux.go are
// generated ahead of time (tools/gen-cvi-types.sh) so the shipped binary stays
// CGO_ENABLED=0 and static.
package cvi

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Device nodes created by the soph-media modules. Each encoder channel is its
// own minor -- cvi_vc_drv_venc_ioctl() takes the channel from iminor(), not
// from the ioctl argument -- so driving channel N means opening EncoderDev+N
// and every ioctl on that handle implicitly addresses that channel.
const (
	SysDev     = "/dev/cvi-sys"  // bind/unbind, cache maintenance
	BaseDev    = "/dev/cvi-base" // mmap window onto physical (ION) memory
	VIDev      = "/dev/cvi-vi"
	VPSSDev    = "/dev/cvi-vpss"
	EncoderDev = "/dev/cvi_vc_enc" // per-channel: cvi_vc_enc0, cvi_vc_enc1, ...
)

// ioctl request numbers.
//
// The vendor uses _IO(type, nr) throughout -- direction NONE and size 0 -- so
// the request is just (type << 8) | nr and carries no size check. The kernel
// side copy_from_user()s a fixed number of bytes based on the command alone,
// which is why the struct layouts have to be exactly right: a short struct
// reads past the end of the Go allocation.
const vencIOCMagic = 'V'

// vencIO builds a VENC request number. Named rather than inlined so the
// derivation stays visible next to the comment above; also avoids shadowing
// the io package with a helper called io.
func vencIO(nr uintptr) uintptr { return vencIOCMagic<<8 | nr }

// VENC channel ioctls (cvi_vc_drv_ioctl.h). Only the ones this package uses
// are named; the full set runs to nr=44.
var (
	vencCreateChn      = vencIO(0)
	vencDestroyChn     = vencIO(1)
	vencResetChn       = vencIO(2)
	vencStartRecvFrame = vencIO(3)
	vencStopRecvFrame  = vencIO(4)
	vencQueryStatus    = vencIO(5)
	vencSetChnAttr     = vencIO(6)
	vencGetChnAttr     = vencIO(7)
	vencGetStream      = vencIO(8)
	vencReleaseStream  = vencIO(9)
	vencSendFrame      = vencIO(11)
	vencRequestIDR     = vencIO(13)
	vencSetJpegParam   = vencIO(24)
	vencSetRcParam     = vencIO(27)
)

// ErrNotPresent reports that the capture stack is not available on this
// system -- the soph-media modules are not loaded, or this is not an SG2002.
// Callers should treat it as "run without video", not as a fatal error.
var ErrNotPresent = errors.New("cvi: multimedia devices not present")

// ioctl issues a request whose argument is a pointer to arg, which is how
// every CVITek command is shaped: the driver copy_from_user()s the bare
// struct, with no wrapper and no size field.
//
// arg must not contain Go pointers that the kernel will follow. The one
// command that does embed a pointer (GET_STREAM, via VENCStreamEx.PstStream)
// is handled in venc.go, which keeps the pointee alive across the call.
func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	r1, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, uintptr(arg))
	// The argument is reachable only through a uintptr for the duration of
	// the syscall, so pin it until the call returns.
	runtime.KeepAlive(arg)
	return status(r1, errno)
}

// ioctlNoArg issues a request that takes no argument (DESTROY_CHN,
// STOP_RECV_FRAME, ...). The driver ignores arg entirely for these.
func ioctlNoArg(f *os.File, req uintptr) error {
	r1, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, 0)
	return status(r1, errno)
}

// status turns an ioctl return into an error.
//
// Both halves matter. errno covers the kernel's own failures (a bad fd, a
// command the driver does not implement). The return value covers the vendor
// API's, because these handlers return CVI_S32 rather than a negative errno:
// cvi_vc_drv_venc_ioctl() ends every case with `return s32Ret`, where s32Ret is
// CVI_SUCCESS or a packed code such as 0xC0078005. Linux only maps a return
// into errno when it falls in [-4095, -1], and a vendor code does not, so the
// syscall reports success and delivers the failure as r1.
//
// This is not theoretical: creating a VENC channel and then reading its
// attributes back appeared to succeed while the driver was logging
// "s32Ret = -1073250299" (venc: resource does not exist) for the same call.
// Anything checking errno alone silently believes the pipeline came up.
func status(r1 uintptr, errno unix.Errno) error {
	if errno != 0 {
		return errno
	}
	// The driver's CVI_S32 is sign-extended into a long on the way out, so
	// take the low word back as int32 to recover the packed code.
	if code := int32(uint32(r1)); code != 0 {
		return Errno(code)
	}
	return nil
}

// openDev opens a pipeline device, mapping ENOENT to ErrNotPresent so a
// missing capture stack is distinguishable from a real failure.
func openDev(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotPresent, path)
		}
		return nil, fmt.Errorf("cvi: open %s: %w", path, err)
	}
	return f, nil
}
