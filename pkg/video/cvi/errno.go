package cvi

import "fmt"

// Errno is a CVITek API status code.
//
// It has to exist because these drivers do not use errno. Every handler in
// cvi_vc_drv_venc_ioctl() returns the vendor API's own CVI_S32 straight out of
// the ioctl -- CVI_SUCCESS (0) or a packed code like 0xC0078005 -- and Linux
// only converts a return value into errno when it lands in [-4095, -1]. A
// vendor code is far outside that window, so the syscall reports success and
// hands the failure back as the return value instead. Checking errno alone
// makes every vendor-level failure look like it worked.
//
// The packing is CVI_DEF_ERR (cvi_errno.h):
//
//	bits 31..30  0b11
//	bits 29..24  app id
//	bits 23..16  module id (MOD_ID_E, BASE=0 ... VENC=7 ...)
//	bits 15..13  level
//	bits 12..0   error id
type Errno int32

// Module reports which subsystem raised the error.
func (e Errno) Module() uint8 { return uint8(uint32(e) >> 16) }

// Level reports the vendor's severity field. It is informational: the codes
// this package sees in practice are all "error" level.
func (e Errno) Level() uint8 { return uint8((uint32(e) >> 13) & 0x7) }

// ID reports the error id within the module. Ids below 64 are the shared set
// in EN_ERR_CODE_E; higher ones are module-private.
func (e Errno) ID() uint16 { return uint16(uint32(e) & 0x1fff) }

func (e Errno) Error() string {
	mod := moduleName(e.Module())
	if name, ok := commonErrNames[e.ID()]; ok {
		return fmt.Sprintf("%s: %s (0x%08x)", mod, name, uint32(e))
	}
	return fmt.Sprintf("%s: error %d (0x%08x)", mod, e.ID(), uint32(e))
}

// Is lets callers match on the shared error ids across modules, so
// errors.Is(err, ErrUnexist) works whether VENC or VPSS raised it.
func (e Errno) Is(target error) bool {
	t, ok := target.(Errno)
	if !ok {
		return false
	}
	return e.ID() == t.ID()
}

// Shared error ids (EN_ERR_CODE_E, cvi_errno.h). Only the ones worth branching
// on are named; the rest render by number.
//
// These carry no module, so they are for comparison via errors.Is, not for
// returning.
const (
	ErrInvalidDevID  Errno = 1
	ErrInvalidChnID  Errno = 2
	ErrIllegalParam  Errno = 3
	ErrExist         Errno = 4
	ErrUnexist       Errno = 5
	ErrNullPtr       Errno = 6
	ErrNotConfig     Errno = 7
	ErrNotSupport    Errno = 8
	ErrNotPerm       Errno = 9
	ErrNoMem         Errno = 12
	ErrNoBuf         Errno = 13
	ErrBufEmpty      Errno = 14
	ErrBufFull       Errno = 15
	ErrSysNotReady   Errno = 16
	ErrBadAddr       Errno = 17
	ErrBusy          Errno = 18
	ErrSizeNotEnough Errno = 19
	ErrInvalidVB     Errno = 20
)

// Module-private ids. VENC's second error block starts at 64
// (ENUM_ERR_VENC_VENC_INIT, cvi_comm_venc.h) and runs in declaration order, so
// these are counted from there rather than being magic numbers.
//
// Only the ones the frame loop has to branch on are named. They matter because
// they are not failures: a console that is not changing produces no frames, and
// GetStream saying so must not be mistaken for the pipeline having died.
const (
	ErrVencInit             Errno = 64
	ErrVencFrcNoEnc         Errno = 65
	ErrVencStatVfpsChange   Errno = 66
	ErrVencEmptyStreamFrame Errno = 67
	ErrVencEmptyPack        Errno = 68
)

var commonErrNames = map[uint16]string{
	1:  "invalid device id",
	2:  "invalid channel id",
	3:  "illegal parameter",
	4:  "resource exists",
	5:  "resource does not exist",
	6:  "null pointer",
	7:  "not configured",
	8:  "not supported",
	9:  "not permitted",
	10: "invalid pipe id",
	11: "invalid group id",
	12: "out of memory",
	13: "out of buffers",
	14: "buffer empty",
	15: "buffer full",
	16: "system not ready",
	17: "bad address",
	18: "busy",
	19: "buffer too small",
	20: "invalid vb handle",
}

// moduleNames is MOD_ID_E in declaration order (FOREACH_MOD, cvi_common.h).
var moduleNames = [...]string{
	"base", "vb", "sys", "rgn", "chnl", "vdec", "vpss", "venc",
	"h264e", "jpege", "mpeg4e", "h265e", "jpegd", "vo", "vi", "dis",
	"rc", "aio", "ai", "ao", "aenc", "adec", "aud", "vpu",
	"isp", "ive", "user", "proc", "log", "h264d", "gdc", "photo", "fb",
}

func moduleName(id uint8) string {
	if int(id) < len(moduleNames) {
		return moduleNames[id]
	}
	return fmt.Sprintf("module %d", id)
}
