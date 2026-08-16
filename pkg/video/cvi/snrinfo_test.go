package cvi

import (
	"testing"
	"unsafe"
)

// The driver copy_from_user()s a fixed number of bytes based on the command
// alone -- these ioctls carry no size -- so a layout that disagrees with the C
// struct is not a type error, it is a field read from the wrong offset. The
// pad byte between raw_num and color_mode is the one that matters: leaving it
// out shifts everything from color_mode on by two bytes, which lands active_w
// in the middle of two other fields and silently reads zero.
func TestSnrInfoLayout(t *testing.T) {
	// struct active_size_s: eight u16, no padding.
	if got := unsafe.Sizeof(ActiveSize{}); got != 16 {
		t.Errorf("ActiveSize size = %d, want 16", got)
	}
	// struct wdr_size_s: u32 frm_num + two active_size_s.
	if got := unsafe.Sizeof(WDRSize{}); got != 36 {
		t.Errorf("WDRSize size = %d, want 36", got)
	}
	// struct cvi_isp_snr_info: u8 + pad + u16 + u32 + wdr_size_s.
	if got := unsafe.Sizeof(SnrInfo{}); got != 44 {
		t.Errorf("SnrInfo size = %d, want 44", got)
	}
	if got := unsafe.Offsetof(SnrInfo{}.PixelRate); got != 4 {
		t.Errorf("SnrInfo.PixelRate offset = %d, want 4", got)
	}
	if got := unsafe.Offsetof(SnrInfo{}.SnrFmt); got != 8 {
		t.Errorf("SnrInfo.SnrFmt offset = %d, want 8", got)
	}
}

// _vi_ctrl_init only programs the CSI bridge when active_w is non-zero, so a
// spec that produced a zero there would reproduce the exact failure this code
// exists to fix, without any error to show for it.
func TestSetupSnrInfoGeometry(t *testing.T) {
	c := &Capturer{}
	s := pipeSpec{inW: 1920, inH: 1080}

	info := c.snrInfoFor(s)
	img := info.SnrFmt.ImgSize[0]

	if img.ActiveW == 0 || img.ActiveH == 0 {
		t.Fatalf("active size is %dx%d; a zero here disables the bridge config entirely",
			img.ActiveW, img.ActiveH)
	}
	if img.Width != 1920 || img.Height != 1080 {
		t.Errorf("csibdg size = %dx%d, want 1920x1080", img.Width, img.Height)
	}
	if img.ActiveW != 1920 || img.ActiveH != 1080 {
		t.Errorf("active size = %dx%d, want 1920x1080", img.ActiveW, img.ActiveH)
	}
	// frm_num > 1 makes the driver read img_size[1] and turn HDR on for the
	// whole pipe, which is not what an HDMI source is.
	if info.SnrFmt.FrmNum != 1 {
		t.Errorf("FrmNum = %d, want 1 (linear)", info.SnrFmt.FrmNum)
	}
	// 9 and 11 are the RGBIR patterns that flip is_rgbir_sensor.
	if info.ColorMode == 9 || info.ColorMode == 11 {
		t.Errorf("ColorMode = %d selects an RGBIR pipeline", info.ColorMode)
	}
}
