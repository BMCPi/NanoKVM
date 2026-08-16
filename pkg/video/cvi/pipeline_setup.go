package cvi

import (
	"fmt"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
)

// This file owns the order in which the capture pipeline is assembled and
// pulled apart. It is separate from capturer.go because the ordering is the
// hard part -- see teardown() for why getting it wrong is a kernel oops rather
// than an error return.

// Fixed pipeline coordinates.
//
// A KVM captures exactly one input into exactly one encoded stream, so nothing
// here needs to be a parameter: one MIPI device, one VI device/pipe/channel,
// one VPSS group/channel, one VENC channel.
const (
	mipiDev  uint32 = 0
	viDev    int32  = 0
	viPipe   int32  = 0
	viChn    int32  = 0
	vpssGrp  int32  = 0
	vpssChn  int32  = 0
	vencChn  int    = 0
	vpssDevN uint8  = 0
)

// Enum values transcribed from the vendor uapi headers. Each is the "no
// special handling" member of its enum, which is what a straight HDMI capture
// wants; they are spelled out rather than left as bare zeros so a reader can
// tell a deliberate default from an unset field.
const (
	videoFormatLinear   uint32 = 0 // VIDEO_FORMAT_LINEAR
	compressModeNone    uint32 = 0 // COMPRESS_MODE_NONE
	dynamicRangeSDR8    uint32 = 0 // DYNAMIC_RANGE_SDR8
	dataBitWidth8       uint32 = 0 // DATA_BITWIDTH_8
	wdrModeNone         uint32 = 0 // WDR_MODE_NONE
	aspectRatioNone     uint32 = 0 // ASPECT_RATIO_NONE
	h264ProfileBaseline uint32 = 0
)

// pipeSpec is everything the bring-up needs to know: what the bridge is
// sending, and what the caller wants out the far end.
type pipeSpec struct {
	// inW/inH is the input timing the LT6911 reports.
	inW, inH int
	// outW/outH is the encoded size. The scaler downsizes, so this bounds
	// bitrate and encoder load independently of what the host sends.
	outW, outH int

	fps     int
	bitrate int
	gop     int
	codec   video.Codec
}

// bringUp assembles the pipeline: MIPI receiver, VI, VPSS, VENC, then the two
// binds that let frames move between them in kernel space.
//
// Every stage is configured before anything is started, and the binds are made
// before the encoder is told to receive: a bind established after StartRecvFrame
// leaves the encoder's handler thread running against a source that is not
// there yet, which is the same lifetime hazard teardown() is careful about at
// the other end.
func (c *Capturer) bringUp(s pipeSpec) error {
	// VB first, and not optionally. VI and VPSS take their frame buffers
	// from the common pools, and they do not check: vpss_set_chn_attr walks
	// find_vb_pool -> isPoolInited and dereferences a NULL pool array if VB
	// was never brought up, which is a kernel oops in the caller's own
	// syscall rather than an error return.
	if err := c.ensureVB(); err != nil {
		return err
	}

	// Point the MIPI RX pads at their MIPI function before configuring the
	// receiver on top of them. U-Boot leaves several of them driving SPI1 and
	// CAM_MCLK1 instead; see setupPinmux.
	if err := setupPinmux(); err != nil {
		return err
	}

	// VI device before the receiver, as the vendor does; see startVIDev.
	if err := c.startVIDev(s); err != nil {
		return err
	}
	if err := c.setupMipi(s); err != nil {
		return err
	}

	// Put the bridge on the lanes now that the receiver is configured and out
	// of reset, and before VI starts looking for data. Nothing downstream
	// starts the transmitter for us -- without this the whole pipeline builds
	// correctly and VI never takes a single interrupt.
	// Sipeed's own bring-up writes no bridge register at all -- lt6911_probe()
	// is the pad mux plus an i2c open, and lt6911_init() is the open on its
	// own -- so on this board the transmitter is expected to free-run. Every
	// write here costs a window open, which halts the firmware driving that
	// transmitter, immediately before VI starts looking for data.
	if _, skip := envUint("NANOKVM_NO_TX_START"); !skip {
		if err := c.bridge.StartOutput(); err != nil {
			return err
		}
		c.txStarted = true
	}

	if err := c.setupVIPipe(s); err != nil {
		return err
	}
	if err := c.setupVPSS(s); err != nil {
		return err
	}
	if err := c.setupVENC(s); err != nil {
		return err
	}

	// VI -> VPSS -> VENC, in kernel space. Once these are up, frames move
	// between stages as VB block handoffs and this process only ever reads
	// the encoded bitstream off the end.
	if err := c.sys.Bind(Chn(ModVI, viDev, viChn), Chn(ModVPSS, vpssGrp, vpssChn)); err != nil {
		return err
	}
	c.boundViVpss = true

	if err := c.sys.Bind(Chn(ModVPSS, vpssGrp, vpssChn), Chn(ModVENC, int32(vencChn), 0)); err != nil {
		return err
	}
	c.boundVpssVenc = true

	// Start the pipe only once the whole chain exists downstream of it.
	if err := c.vi.StartPipe(viPipe); err != nil {
		return err
	}
	c.pipeStarted = true

	if err := c.vpss.StartGroup(vpssGrp); err != nil {
		return err
	}
	c.grpStarted = true

	if err := c.enc.StartRecvFrame(RecvUnlimited); err != nil {
		return err
	}
	c.recvStarted = true

	return nil
}

// VB pool geometry.
//
// The pool is sized for the largest mode the bridge delivers rather than for
// the mode currently on screen, because VB is initialised once for the life of
// the driver: a pool cut to fit 720p would have to be torn down and rebuilt the
// moment the host switched to 1080p, and VB has no way to do that while VI and
// VPSS hold blocks from it. Sizing for the maximum makes a mode change a
// pipeline rebuild rather than a memory-layout change.
const (
	vbMaxW = 1920
	vbMaxH = 1080

	// Enough blocks for VI and VPSS to each hold a couple in flight plus one
	// being written. At the maximum size this is ~25 MiB of the ~105 MiB
	// carveout, leaving room for the encoder's own reference and bitstream
	// buffers.
	vbBlockCount = 8

	// VB_REMAP_MODE_NONE. Nothing here needs a CPU mapping of a frame: the
	// pixels move VI -> VPSS -> VENC entirely in kernel space, and asking
	// for a remap would cost address space and cache maintenance for a
	// mapping no one reads.
	vbRemapNone uint8 = 0
)

// vbBlockSize is the bytes one NV21 frame occupies, with the stride alignment
// the scaler writes at. Guessing low here does not fail cleanly -- the pool
// would be created and allocations from it would come back short -- so the
// alignment is applied rather than assumed away.
func vbBlockSize(w, h int) uint32 {
	const strideAlign = 64
	stride := (w + strideAlign - 1) &^ (strideAlign - 1)
	return uint32(stride * h * 3 / 2)
}

// ensureVB brings the common VB pools up if nothing has already done so.
//
// VB is global to the driver, not to this process: a pool set configured by an
// earlier run survives this one starting, and VBSetConfig is silently ignored
// once VB is up ("vb has already inited, set_config cmd has no effect"). So the
// state is checked rather than assumed, and an existing configuration is left
// alone instead of being fought over.
func (c *Capturer) ensureVB() error {
	inited, err := c.base.VBInited()
	if err != nil {
		return fmt.Errorf("cvi: vb: read init state: %w", err)
	}
	if inited {
		return nil
	}

	var cfg VBCfg
	cfg.Pool_cnt = 1
	cfg.Pool[0] = VBPoolCfg{
		Blk_size:   vbBlockSize(vbMaxW, vbMaxH),
		Blk_cnt:    vbBlockCount,
		Remap_mode: vbRemapNone,
	}

	if err := c.base.VBSetConfig(&cfg); err != nil {
		return err
	}
	return c.base.VBInit()
}

func (c *Capturer) setupMipi(s pipeSpec) error {
	attr := LT6911ComboAttr(mipiDev)
	attr.Img_size = ImgSize{Width: uint32(s.inW), Height: uint32(s.inH)}

	if err := c.mipi.ResetSensor(mipiDev); err != nil {
		return err
	}
	if err := c.mipi.ResetMipi(mipiDev); err != nil {
		return err
	}
	if err := c.mipi.SetDevAttr(attr); err != nil {
		return err
	}
	if err := c.mipi.EnableClock(mipiDev); err != nil {
		return err
	}
	// The vendor's sequence settles for 20us between starting the clock and
	// releasing reset (SAMPLE_COMM_VI_StartMIPI). The receiver latches its
	// configuration on release, so it wants a stable clock first.
	time.Sleep(20 * time.Microsecond)

	// Out of reset last: the receiver latches its configuration on release,
	// so unresetting before SetDevAttr would run it with whatever the
	// previous session left behind.
	return c.mipi.UnresetSensor(mipiDev)
}

// startVIDev configures and enables the VI device.
//
// This runs before the MIPI receiver is brought up, which looks backwards but
// matches the vendor: SAMPLE_COMM_VI_StartDev (CVI_VI_SetDevAttr +
// CVI_VI_EnableDev) is called before SAMPLE_COMM_VI_StartMIPI. The device has
// to be listening on the interface before the receiver starts driving it.
func (c *Capturer) startVIDev(s pipeSpec) error {
	dev := VIDevAttr{
		EnIntfMode:      VIModeMipiYuv422,
		EnWorkMode:      VIWorkMode1Multiplex,
		EnScanMode:      VIScanProgressive,
		EnDataSeq:       VIDataSeqUYVY,
		EnInputDataType: VIDataTypeYUV,
		StSize:          Size{U32Width: uint32(s.inW), U32Height: uint32(s.inH)},
		StWDRAttr:       VIWDRAttr{EnWDRMode: wdrModeNone},
		Num:             1,
		SnrFps:          uint32(s.fps),
	}
	// AdChnId is an analogue-decoder channel map; -1 in every slot is the
	// vendor's "not an analogue input".
	dev.As32AdChnId = [4]int32{-1, -1, -1, -1}

	if err := c.vi.SetDevAttr(viDev, &dev); err != nil {
		return err
	}
	if err := c.vi.EnableDev(viDev); err != nil {
		return err
	}
	c.devEnabled = true
	return nil
}

// setupVIPipe creates the pipe and channel, once the receiver is running.
func (c *Capturer) setupVIPipe(s pipeSpec) error {
	pipe := VIPipeAttr{
		EnPipeBypassMode: VIPipeBypassNone,
		U32MaxW:          uint32(s.inW),
		U32MaxH:          uint32(s.inH),
		EnPixFmt:         PixelFormatNV21,
		EnCompressMode:   compressModeNone,
		EnBitWidth:       dataBitWidth8,
		StFrameRate:      FrameRateCtrl{S32SrcFrameRate: int32(s.fps), S32DstFrameRate: int32(s.fps)},
		// The bridge already delivers YUV, so the ISP has nothing to do:
		// bypassing it skips the Bayer stages instead of running them over
		// data they were never meant to see.
		BYuvBypassPath: 1,
	}
	if err := c.vi.CreatePipe(viPipe, &pipe); err != nil {
		return err
	}
	c.pipeCreated = true

	chn := VIChnAttr{
		StSize:         Size{U32Width: uint32(s.inW), U32Height: uint32(s.inH)},
		EnPixelFormat:  PixelFormatNV21,
		EnDynamicRange: dynamicRangeSDR8,
		EnVideoFormat:  videoFormatLinear,
		EnCompressMode: compressModeNone,
		StFrameRate:    FrameRateCtrl{S32SrcFrameRate: int32(s.fps), S32DstFrameRate: int32(s.fps)},
	}
	if err := c.vi.SetChnAttr(viPipe, viChn, &chn); err != nil {
		return err
	}
	if err := c.vi.EnableChn(viPipe, viChn); err != nil {
		return err
	}
	c.viChnEnabled = true
	return nil
}

func (c *Capturer) setupVPSS(s pipeSpec) error {
	grp := VPSSGrpAttr{
		U32MaxW:       uint32(s.inW),
		U32MaxH:       uint32(s.inH),
		EnPixelFormat: PixelFormatNV21,
		StFrameRate:   FrameRateCtrl{S32SrcFrameRate: int32(s.fps), S32DstFrameRate: int32(s.fps)},
		U8VpssDev:     vpssDevN,
	}
	if err := c.vpss.CreateGroup(vpssGrp, &grp); err != nil {
		return err
	}
	c.grpCreated = true

	chn := VPSSChnAttr{
		U32Width:      uint32(s.outW),
		U32Height:     uint32(s.outH),
		EnVideoFormat: videoFormatLinear,
		EnPixelFormat: PixelFormatNV21,
		StFrameRate:   FrameRateCtrl{S32SrcFrameRate: int32(s.fps), S32DstFrameRate: int32(s.fps)},
		StAspectRatio: AspectRatio{EnMode: aspectRatioNone},
	}
	if err := c.vpss.SetChnAttr(vpssGrp, vpssChn, &chn); err != nil {
		return err
	}
	if err := c.vpss.EnableChn(vpssGrp, vpssChn); err != nil {
		return err
	}
	c.vpssChnEnabled = true
	return nil
}

func (c *Capturer) setupVENC(s pipeSpec) error {
	payload, rcMode, err := codecParams(s.codec)
	if err != nil {
		return err
	}

	attr := VENCChnAttr{
		StVencAttr: VENCAttr{
			EnType:          payload,
			U32MaxPicWidth:  uint32(s.outW),
			U32MaxPicHeight: uint32(s.outH),
			U32PicWidth:     uint32(s.outW),
			U32PicHeight:    uint32(s.outH),
			U32BufSize:      uint32(bitstreamBufSize(s.outW, s.outH)),
			U32Profile:      h264ProfileBaseline,
			BByFrame:        1, // one GetStream per frame, not per packet
			BSingleCore:     1,
		},
		StRcAttr: VENCRcAttr{
			EnRcMode: rcMode,
			StH264Cbr: VENCH264Cbr{
				U32Gop:           uint32(s.gop),
				U32StatTime:      1,
				U32SrcFrameRate:  uint32(s.fps),
				Fr32DstFrameRate: uint32(s.fps),
				U32BitRate:       uint32(s.bitrate / 1000), // driver takes kbps
			},
		},
		StGopAttr: VENCGopAttr{
			EnGopMode: VencGopModeNormalP,
			StNormalP: VENCGopNormalP{S32IPQpDelta: 2},
		},
	}

	if err := c.enc.CreateChn(&attr); err != nil {
		return err
	}
	c.chnCreated = true
	return nil
}

// codecParams maps the platform-neutral codec onto the payload type and the
// rate-control mode, which the driver requires to agree: an H.265 channel with
// an H.264 RC mode is rejected outright.
func codecParams(c video.Codec) (payload, rcMode uint32, err error) {
	switch c {
	case video.CodecH264:
		return PTH264, VencRCModeH264CBR, nil
	case video.CodecH265:
		return PTH265, VencRCModeH265CBR, nil
	case video.CodecMJPEG:
		return PTMJPEG, VencRCModeMJPEGCBR, nil
	default:
		return 0, 0, fmt.Errorf("cvi: unsupported codec %v", c)
	}
}

// bitstreamBufSize sizes the encoder's output ring.
//
// The vendor sizes this from the frame area. A console is mostly static so the
// average frame is tiny, but the ring has to survive the worst case -- the IDR
// sent when a viewer joins on a screen full of text -- without stalling the
// encoder, and an undersized ring shows up as dropped frames under exactly the
// conditions a KVM is being watched.
func bitstreamBufSize(w, h int) int {
	const minBuf = 1 << 20
	n := w * h / 2
	if n < minBuf {
		return minBuf
	}
	return n
}

// teardown pulls the pipeline apart in the reverse of the order it was built,
// and returns the first error while continuing regardless.
//
// The order between unbind, StopRecvFrame and DestroyChn is not stylistic. In
// cvi_venc.c the only kthread_stop for the bind-mode handler lives in
// CVI_VENC_StopRecvFrame and is gated on IF_WANNA_DISABLE_BIND_MODE(), which
// requires enable_bind_mode to already be false -- and that flag is cleared by
// sys's unbind callback. Destroy a channel that is still bound and the stop is
// skipped, so DestroyChn vfree()s the channel context out from under a running
// thread and the next wakeup takes a kernel oops in venc_event_handler.
//
// So: unbind first, then stop receiving, then destroy. The driver in this tree
// also carries a kthread_stop in DestroyChn as a backstop, but that fix cannot
// be relied on by a binary that might run against a stock vendor build.
func (c *Capturer) teardown() error {
	var firstErr error
	fail := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 1. Unbind, so the encoder's handler learns bind mode is going away.
	if c.boundVpssVenc {
		fail(c.sys.Unbind(Chn(ModVPSS, vpssGrp, vpssChn), Chn(ModVENC, int32(vencChn), 0)))
		c.boundVpssVenc = false
	}
	if c.boundViVpss {
		fail(c.sys.Unbind(Chn(ModVI, viDev, viChn), Chn(ModVPSS, vpssGrp, vpssChn)))
		c.boundViVpss = false
	}

	// 2. Stop receiving. Now that unbind has run, this is what joins the
	//    handler thread.
	if c.recvStarted {
		fail(c.enc.StopRecvFrame())
		c.recvStarted = false
	}

	// 3. Only now is it safe to free the channel.
	if c.chnCreated {
		fail(c.enc.DestroyChn())
		c.chnCreated = false
	}

	if c.grpStarted {
		fail(c.vpss.StopGroup(vpssGrp))
		c.grpStarted = false
	}
	if c.vpssChnEnabled {
		fail(c.vpss.DisableChn(vpssGrp, vpssChn))
		c.vpssChnEnabled = false
	}
	if c.grpCreated {
		fail(c.vpss.DestroyGroup(vpssGrp))
		c.grpCreated = false
	}

	if c.pipeStarted {
		fail(c.vi.StopPipe(viPipe))
		c.pipeStarted = false
	}
	if c.viChnEnabled {
		fail(c.vi.DisableChn(viPipe, viChn))
		c.viChnEnabled = false
	}
	if c.pipeCreated {
		fail(c.vi.DestroyPipe(viPipe))
		c.pipeCreated = false
	}
	if c.devEnabled {
		fail(c.vi.DisableDev(viDev))
		c.devEnabled = false
	}

	fail(c.mipi.DisableClock(mipiDev))

	// Take the bridge off the lanes last, once nothing downstream is still
	// expecting data from it.
	if c.txStarted {
		fail(c.bridge.StopOutput())
		c.txStarted = false
	}
	return firstErr
}
