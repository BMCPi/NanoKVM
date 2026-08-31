package optee

import (
	"testing"
	"unsafe"
)

// The numbers below were read out of the kernel's own linux/tee.h by
// compiling it, not derived by hand:
//
//	sizeof version_data      = 12    TEE_IOC_VERSION       = 0x800ca400
//	sizeof buf_data          = 16    TEE_IOC_OPEN_SESSION  = 0x8010a402
//	sizeof param             = 32    TEE_IOC_INVOKE        = 0x8010a403
//	sizeof open_session_arg  = 56    TEE_IOC_CANCEL        = 0x8008a404
//	sizeof invoke_arg        = 24    TEE_IOC_CLOSE_SESSION = 0x8004a405
//	sizeof cancel_arg        = 8
//	sizeof close_session_arg = 4
//	offsetof(open_session_arg, params) = 56
//	offsetof(invoke_arg, params)       = 24
//
// Every one of these crosses the kernel boundary. A Go struct that drifts
// from its C original corrupts whatever follows it, so the layouts are
// asserted rather than trusted.

func TestStructSizesMatchKernelABI(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"versionData", unsafe.Sizeof(versionData{}), 12},
		{"bufData", unsafe.Sizeof(bufData{}), 16},
		{"ioctlParam", unsafe.Sizeof(ioctlParam{}), 32},
		{"openSessionArg", unsafe.Sizeof(openSessionArg{}), 56},
		{"invokeArg", unsafe.Sizeof(invokeArg{}), 24},
		{"cancelArg", unsafe.Sizeof(cancelArg{}), 8},
		{"closeSessionArg", unsafe.Sizeof(closeSessionArg{}), 4},
	} {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// The flexible params[] tail has to land where C puts it. Go is free to pad
// a struct before an 8-aligned array; here it happens not to, and this is
// what says so out loud.
func TestParamArrayLandsAtTheKernelOffset(t *testing.T) {
	if got := unsafe.Offsetof(openSessionBuf{}.Params); got != 56 {
		t.Errorf("openSessionBuf.Params offset = %d, want 56", got)
	}
	if got := unsafe.Offsetof(invokeBuf{}.Params); got != 24 {
		t.Errorf("invokeBuf.Params offset = %d, want 24", got)
	}
	// The whole buffer is header + params with nothing after it, which is
	// the length handed to the kernel in bufData.BufLen.
	if got, want := unsafe.Sizeof(openSessionBuf{}), uintptr(56+paramCount*32); got != want {
		t.Errorf("sizeof(openSessionBuf) = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(invokeBuf{}), uintptr(24+paramCount*32); got != want {
		t.Errorf("sizeof(invokeBuf) = %d, want %d", got, want)
	}
}

func TestIoctlRequestNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"TEE_IOC_VERSION", reqVersion(), 0x800ca400},
		{"TEE_IOC_OPEN_SESSION", reqOpenSession(), 0x8010a402},
		{"TEE_IOC_INVOKE", reqInvoke(), 0x8010a403},
		{"TEE_IOC_CANCEL", reqCancel(), 0x8008a404},
		{"TEE_IOC_CLOSE_SESSION", reqCloseSession(), 0x8004a405},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = 0x%08x, want 0x%08x", tc.name, tc.got, tc.want)
		}
	}
}

// The device scan must not pick up teepriv0: that node belongs to the
// supplicant and rejects the client ioctls, so matching it would turn "no
// OP-TEE" into a confusing ENOTTY from the first call.
func TestDevicePatternSkipsSupplicantNode(t *testing.T) {
	for _, name := range []string{"tee0", "tee1", "tee10"} {
		if !devicePattern.MatchString(name) {
			t.Errorf("%s should be treated as a client device", name)
		}
	}
	for _, name := range []string{"teepriv0", "tee", "teeX", "ttyS1", "atee0"} {
		if devicePattern.MatchString(name) {
			t.Errorf("%s should not be treated as a client device", name)
		}
	}
}
