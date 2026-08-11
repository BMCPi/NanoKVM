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
#include <linux/cvi_base.h>
#include <linux/vb_uapi.h>
#include <linux/cif_uapi.h>
#include <linux/vi_uapi.h>
#include <linux/vpss_uapi.h>
#include <linux/sys_uapi.h>

// VIDEO_FRAME_INFO_EX_S is the SEND_FRAME ioctl argument. Like
// VENC_STREAM_EX_S it lives only in the driver-private cvi_vc_drv.h, so
// repeat it here. Keep in sync with cvi_vc_drv.h.
typedef struct _VIDEO_FRAME_INFO_EX_S {
	VIDEO_FRAME_INFO_S *pstFrame;
	CVI_S32 s32MilliSec;
} VIDEO_FRAME_INFO_EX_S;

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

// --- base/vb: physically contiguous frame buffers ---
//
// The encoder reads frames by physical address out of the ION carveout, so
// anything feeding it from userspace has to allocate through VB rather than
// with make([]byte). vb_ext_control is the envelope every VB command travels
// in: IOCTL_VB_CMD on /dev/cvi-base, with id naming the VB_IOCTL and the union
// carrying a pointer to the command's own struct.

type VBExtControl C.struct_vb_ext_control

type VBPoolCfg C.struct_cvi_vb_pool_cfg

type VBCfg C.struct_cvi_vb_cfg

type VBBlkCfg C.struct_cvi_vb_blk_cfg

type VBBlkInfo C.struct_cvi_vb_blk_info

// VideoFrameInfoEx is the SEND_FRAME argument -- a pointer to a frame plus a
// timeout, the same shape as VENCStreamEx.

type VideoFrameInfoEx C.VIDEO_FRAME_INFO_EX_S

// --- cif: the MIPI CSI-2 receiver in front of VI ---
//
// ComboDevAttr is what tells the receiver how to interpret the lanes coming
// off the HDMI bridge. It is a union over MIPI/LVDS/parallel attributes, so
// the MIPI half is named separately to be reachable.

type ComboDevAttr C.struct_combo_dev_attr_s

type MipiDevAttr C.struct_mipi_dev_attr_s

type DPhy C.struct_dphy_s

type MipiDemuxInfo C.struct_mipi_demux_info_s

// Nested members of ComboDevAttr. Unused by this package but named because
// godefs emits a dangling _Ctype_struct_ reference for any struct it reaches
// through a field and cannot name.

type MclkPll C.struct_mclk_pll_s

type ImgSize C.struct_img_size_s

type ManualWdrAttr C.struct_manual_wdr_attr_s

// --- vi: capture device, pipe and channel ---
//
// VI has two command spaces over one pair of ioctls. VIExtControl.Id selects
// an ISP-side VI_IOCTL, while SdkId selects a VI_SDK_CTRL -- the dev/pipe/chn
// lifecycle this package drives. SdkCfg carries which dev, pipe and channel
// the command applies to, plus the pointer to its own argument.

type VIExtControl C.struct_vi_ext_control

type VISdkCfg C.struct__vi_sdk_cfg

type VIDevAttr C.VI_DEV_ATTR_S

type VIPipeAttr C.VI_PIPE_ATTR_S

type VIChnAttr C.VI_CHN_ATTR_S

type VIWDRAttr C.VI_WDR_ATTR_S

type VISyncCfg C.VI_SYNC_CFG_S

type VITimingBlank C.VI_TIMING_BLANK_S

// --- vpss: scaler and format converter ---

type VPSSCreateGrpCfg C.struct_vpss_crt_grp_cfg

type VPSSStartGrpCfg C.struct_vpss_str_grp_cfg

type VPSSGrpAttrCfg C.struct_vpss_grp_attr

type VPSSChnAttrCfg C.struct_vpss_chn_attr

type VPSSEnChnCfg C.struct_vpss_en_chn_cfg

// --- sys: wiring the stages together ---
//
// SysBindCfg is the whole reason the capture path never enters userspace:
// binding VI -> VPSS -> VENC hands frames between stages inside the kernel,
// and this process only ever sees the encoded bitstream at the end.

type SysBindCfg C.struct_sys_bind_cfg
