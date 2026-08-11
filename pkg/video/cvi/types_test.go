package cvi

import (
	"testing"
	"unsafe"
)

// The generated structs are passed straight to the kernel by pointer, so a
// layout mismatch is not a compile error -- it is silent memory corruption in
// the driver. These sizes were measured by compiling the vendor headers with
// the Yocto riscv64 cross toolchain (see tools/gen-cvi-types.sh) and are
// pinned here so that regenerating against drifted headers fails loudly
// instead of quietly changing the ABI.
//
// If one of these fires after a soph-media bump, do not just update the
// number: work out which vendor struct changed and whether the driver in the
// image changed with it.
func TestStructSizesMatchVendorABI(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"MMFChn", unsafe.Sizeof(MMFChn{}), 12},
		{"VENCChnAttr", unsafe.Sizeof(VENCChnAttr{}), 112},
		{"VENCPack", unsafe.Sizeof(VENCPack{}), 144},
		{"VENCRecvPicParam", unsafe.Sizeof(VENCRecvPicParam{}), 4},
		{"VENCStream", unsafe.Sizeof(VENCStream{}), 384},
		{"VideoFrameInfo", unsafe.Sizeof(VideoFrameInfo{}), 152},
		{"VPSSChnAttr", unsafe.Sizeof(VPSSChnAttr{}), 92},
		{"VPSSGrpAttr", unsafe.Sizeof(VPSSGrpAttr{}), 24},
	} {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, vendor ABI says %d", tc.name, tc.got, tc.want)
		}
	}
}

// VENCStreamEx is hand-declared in gen_types.go's preamble rather than pulled
// from a header (the vendor puts it in a driver-private file). That makes it
// the single most likely place for the ioctl ABI to silently drift, so check
// its shape explicitly: a pointer followed by an int32, tail-padded to 16 on
// LP64.
func TestVENCStreamExLayout(t *testing.T) {
	t.Parallel()

	var s VENCStreamEx
	if got, want := unsafe.Sizeof(s), uintptr(16); got != want {
		t.Errorf("sizeof(VENCStreamEx) = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(s.PstStream), uintptr(0); got != want {
		t.Errorf("offsetof(PstStream) = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(s.S32MilliSec), uintptr(8); got != want {
		t.Errorf("offsetof(S32MilliSec) = %d, want %d", got, want)
	}
}
