package cvi

import (
	"fmt"
	"unsafe"
)

// The sensor geometry the ISP sizes its CSI bridge from.
//
// VI_SetDevAttr tells the driver what the *device* looks like, but it is not
// where the ISP gets the numbers it programs the CSI bridge with. Those come
// from a separate block the sensor driver pushes down, and _vi_ctrl_init will
// not touch the bridge unless that block is populated (vi.c:2487):
//
//	if (vdev->snr_info[raw_num].snr_fmt.img_size[0].active_w != 0) {
//	        ictx->isp_pipe_cfg[raw_num].csibdg_width  = ...img_size[0].width;
//	        ictx->isp_pipe_cfg[raw_num].csibdg_height = ...img_size[0].height;
//	        ...
//	}
//
// Without it csibdg_width stays 0, and the failure is loud but misleading: the
// bridge compares every arriving frame against a configured width of zero,
// fails the check, and the error handler resets the ISP and re-triggers preraw
// forever --
//
//	isp_err_chk: CSIBDG_A CH0 frm width less than setting(0)
//	_vi_err_retrig_preraw: fe_0 isp_pre_trig retry 27 times
//	isp_frm_err_handler: ISP and vip_sys isp rst pull down
//
// -- so the link is decoding real video, the counters are clean, and VI still
// never completes a frame. The "(0)" in that message is the *configured* width
// being compared against, not a measured one, which is what gives the game
// away.
//
// The vendor sends this from its sensor driver, which is why it is missing
// here for the same reason the pad mux was: that code lives in libsns, and an
// HDMI bridge is not a sensor. Everything the driver actually reads is
// geometry, so it can be supplied directly from the input timing.
const viIOCTLSetSnrInfo uint32 = 20

// ActiveSize is struct active_size_s (vi_snsr.h). All u16, no padding.
type ActiveSize struct {
	Width     uint16
	Height    uint16
	StartX    uint16
	StartY    uint16
	ActiveW   uint16
	ActiveH   uint16
	MaxWidth  uint16
	MaxHeight uint16
}

// WDRSize is struct wdr_size_s. img_size is indexed by exposure: [0] is the
// long exposure and the only one a non-HDR source uses.
type WDRSize struct {
	FrmNum  uint32
	ImgSize [2]ActiveSize
}

// SnrInfo is struct cvi_isp_snr_info. The explicit pad is the byte C inserts
// between the u8 and the u16 that follows it; without it every field from
// color_mode on lands two bytes early.
type SnrInfo struct {
	RawNum    uint8
	_         uint8
	ColorMode uint16
	PixelRate uint32
	SnrFmt    WDRSize
}

// SetSnrInfo hands the driver the sensor geometry for one raw pipe.
func (v *VI) SetSnrInfo(info *SnrInfo) error {
	if _, err := v.ctl(viIOCSetCtrl, viIOCTLSetSnrInfo, unsafe.Pointer(info), 0); err != nil {
		return fmt.Errorf("cvi: vi set sensor info: %w", err)
	}
	return nil
}

// setupSnrInfo describes the incoming video to the ISP.
//
// Ordering is not flexible. _vi_ctrl_init consumes this once and then latches
// is_ctrl_inited, and it is reached from two directions: vi_start_streaming
// (vi.c:2959) and, less obviously, the GET_BUF_SIZE handler (vi.c:5925) that
// ISPBufSize calls. So this has to run before the pipe is started *and* before
// the DMA pool is sized, or the first of those to arrive latches a zero width
// that nothing afterwards can correct.
func (c *Capturer) setupSnrInfo(s pipeSpec) error {
	info := c.snrInfoFor(s)
	return c.vi.SetSnrInfo(&info)
}

// snrInfoFor builds the block setupSnrInfo sends, separated out so the
// geometry can be checked without a device.
func (c *Capturer) snrInfoFor(s pipeSpec) SnrInfo {
	w, h := uint16(s.inW), uint16(s.inH)

	return SnrInfo{
		RawNum: uint8(viDev),
		// ISP_BAYER_TYPE_BG. The value is only read for a Bayer source --
		// it becomes rgb_color_mode, which this YUV path never programs --
		// but it must not be one of the RGBIR patterns, because those flip
		// is_rgbir_sensor and change the pipeline shape (vi.c:2517).
		ColorMode: 0,
		// Declared but never read by the driver.
		PixelRate: 0,
		SnrFmt: WDRSize{
			// Linear, not HDR. frm_num > 1 makes the driver read
			// img_size[1] and turn HDR on for the whole pipe.
			FrmNum: 1,
			ImgSize: [2]ActiveSize{{
				// width/height size the CSI bridge; active_* is the
				// crop taken out of it. An HDMI source is already
				// exactly the size it claims, so the crop is the
				// whole frame and there is no blanking to skip.
				Width:     w,
				Height:    h,
				StartX:    0,
				StartY:    0,
				ActiveW:   w,
				ActiveH:   h,
				MaxWidth:  w,
				MaxHeight: h,
			}},
		},
	}
}
