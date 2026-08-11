//go:build generate

// This file is the input to `cgo -godefs`; it is never compiled into the
// binary (see the build tag above). Running it emits types_linux.go,
// which is checked in, so an ordinary build needs neither cgo nor the CVITek
// headers -- only regenerating does.
//
// Regenerate with tools/gen-cvi-types.sh, which points CC at the Yocto
// riscv64 cross toolchain. Do not hand-edit the generated file.
//
// Two shims are needed because the vendor's "uapi" headers are not actually
// self-contained:
//
//   - VB_POOL is referenced by cvi_comm_venc.h but typedef'd only in the
//     kernel-side kapi header base_ctx.h (as uint32_t). Repeating it here is
//     what lets the uapi set compile standalone.
//   - <stdio.h> must precede the vendor headers: cvi_comm_venc.h has a
//     FILE *dumpYuv member and does not include stdio itself.
//
// The layouts these produce are identical on riscv64 and x86_64 (both LP64
// little-endian, and none of these structs use long double or bitfields), so
// a host-generated file would be byte-identical -- but generating against the
// real target compiler is what makes that a fact rather than an assumption.

package cvi

/*
#include <string.h>
#include <stdio.h>
#include <stdint.h>
#include <pthread.h>

typedef uint32_t VB_POOL;

#include <linux/cvi_buffer.h>
#include <linux/cvi_comm_venc.h>
#include <linux/cvi_comm_vpss.h>
#include <linux/cvi_comm_vi.h>
#include <linux/cvi_comm_sys.h>

// VENC_STREAM_EX_S is the GET_STREAM/RELEASE_STREAM ioctl argument, but it is
// declared in the driver-private cvi_vc_drv/cvi_vc_drv.h rather than anywhere
// under include/, and that header pulls in kernel-only decls. It is two
// fields, so repeat it verbatim here instead. Keep in sync with
// cvi_vc_drv.h:58.
typedef struct _VENC_STREAM_EX_S {
	VENC_STREAM_S *pstStream;
	CVI_S32 s32MilliSec;
} VENC_STREAM_EX_S;
*/
import "C"

// --- sys: module/channel identity, used for binding stages together ---

type MMFChn C.MMF_CHN_S

type MMFBindDest C.MMF_BIND_DEST_S

// --- geometry, shared by VPSS and VENC ---

type Size C.SIZE_S

type Rect C.RECT_S

// --- VPSS: the scaler/format converter between VI and the encoder ---

type VPSSGrpAttr C.VPSS_GRP_ATTR_S

type VPSSChnAttr C.VPSS_CHN_ATTR_S

// --- VENC: encoder channel configuration ---

type VENCChnAttr C.VENC_CHN_ATTR_S

type VENCAttr C.VENC_ATTR_S

type VENCRcAttr C.VENC_RC_ATTR_S

type VENCGopAttr C.VENC_GOP_ATTR_S

type VENCRecvPicParam C.VENC_RECV_PIC_PARAM_S

type VENCChnStatus C.VENC_CHN_STATUS_S

// --- VENC: the output path ---
//
// VENCStreamEx is the argument to the GET_STREAM/RELEASE_STREAM ioctls: it
// carries a pointer to a caller-owned VENCStream plus a timeout. The kernel
// fills the VENCStream and copies u32PackCount packs into the caller's
// pstPack array, so that array must be allocated before the call.

type VENCStreamEx C.VENC_STREAM_EX_S

type VENCStream C.VENC_STREAM_S

type VENCPack C.VENC_PACK_S

// --- frames (only needed for the non-bound, send-frame-by-hand path) ---

type VideoFrameInfo C.VIDEO_FRAME_INFO_S

type VideoFrame C.VIDEO_FRAME_S

// --- nested types ---
//
// These are reached only through other structs -- mostly as the first member
// of a union, which is how cgo -godefs renders a C union. They still have to
// be named explicitly: without a declaration godefs emits a dangling
// _Ctype_struct__FOO reference that does not compile.

type AspectRatio C.ASPECT_RATIO_S

type FrameRateCtrl C.FRAME_RATE_CTRL_S

type VPSSNormalize C.VPSS_NORMALIZE_S

type VENCPackInfo C.VENC_PACK_INFO_S

type VENCStreamInfo C.VENC_STREAM_INFO_S

type VENCSSEInfo C.VENC_SSE_INFO_S

// H.264 is the codec this board streams to browsers, so its variants are the
// ones that surface from the unions in VENCAttr / VENCRcAttr / VENCStream.
// The H.265 and JPEG variants overlay the same storage; add them here if a
// call site needs to read them as such.

type VENCAttrH264 C.VENC_ATTR_H264_S

type VENCH264Cbr C.VENC_H264_CBR_S

type VENCGopNormalP C.VENC_GOP_NORMALP_S

type VENCStreamInfoH264 C.VENC_STREAM_INFO_H264_S

type VENCStreamAdvanceInfoH264 C.VENC_STREAM_ADVANCE_INFO_H264_S
