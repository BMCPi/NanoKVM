package cvi

// Enumerations from the CVITek uapi headers.
//
// These are hand-transcribed rather than generated: cgo -godefs emits struct
// layouts, not enum constants, and adding a generated file per enum would cost
// more than it saves for three short lists. Each block names its source header
// so the values can be re-checked against a bumped vendor tree.

// Payload types (PAYLOAD_TYPE_E, cvi_common.h).
//
// The numbering is RTP-ish but not RTP: PT_H264 = 96 happens to match the
// dynamic RTP payload type, while PT_H265 = 265 and PT_MJPEG = 1002 are
// vendor inventions. Do not reuse these as RTP payload types.
const (
	PTJPEG  uint32 = 26
	PTH264  uint32 = 96
	PTH265  uint32 = 265
	PTMJPEG uint32 = 1002
)

// Rate-control modes (VENC_RC_MODE_E, cvi_comm_rc.h).
//
// The mode has to match the channel's payload type -- an H.265 channel with
// VencRCModeH264CBR is rejected -- which is why the codec is spelled into
// every name rather than being a separate axis.
const (
	VencRCModeH264CBR uint32 = iota + 1
	VencRCModeH264VBR
	VencRCModeH264AVBR
	VencRCModeH264QVBR
	VencRCModeH264FixQP
	VencRCModeH264QPMap
	VencRCModeH264UBR

	VencRCModeMJPEGCBR
	VencRCModeMJPEGVBR
	VencRCModeMJPEGFixQP

	VencRCModeH265CBR
	VencRCModeH265VBR
	VencRCModeH265AVBR
	VencRCModeH265QVBR
	VencRCModeH265FixQP
	VencRCModeH265QPMap
	VencRCModeH265UBR
)

// GOP modes (VENC_GOP_MODE_E, cvi_comm_venc.h).
//
// NormalP is the only one a console stream wants: the others exist to trade
// latency for compression with B-frames or reference pyramids, and a KVM is
// the case where latency is the whole point.
const (
	VencGopModeNormalP uint32 = iota
	VencGopModeDualP
	VencGopModeSmartP
	VencGopModeAdvSmartP
	VencGopModeBiPredB
	VencGopModeLowDelayB
)
