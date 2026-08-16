package cvi

import (
	"fmt"
	"os"
	"unsafe"
)

// Device nodes for the capture stages.
const (
	MipiRxDev = "/dev/cvi-mipi-rx"
)

// ioctl direction bits, asm-generic encoding (what riscv64 uses).
const (
	iocNone  uintptr = 0
	iocWrite uintptr = 1
	iocRead  uintptr = 2
)

// ioc builds an ioctl request number: direction in bits 31..30, argument size
// in 29..16, type in 15..8, command in 7..0.
//
// These are computed rather than transcribed because the vendor's _IOW macros
// encode sizeof(struct), and a struct that grows in a vendored-source bump
// would silently change the request number. Deriving it from the Go type keeps
// the two in step -- the layouts are generated from the same headers.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

// Payload-type and format constants used when configuring the pipeline.
const (
	// VIModeMipiYuv422 is the interface mode for a bridge feeding YUV422
	// over CSI-2, which is what the LT6911 emits (VI_MODE_MIPI_YUV422).
	VIModeMipiYuv422 uint32 = 8
	// VIDataSeqUYVY orders the YUV components as the bridge sends them.
	// VI_YUV_DATA_SEQ_E starts with two 4:2:0 orders (VUVU, UVUV) before the
	// 4:2:2 ones, so this is 2 rather than the 1 a glance at the enum
	// suggests.
	VIDataSeqUYVY uint32 = 2
	// VIDataTypeYUV selects the YUV input path rather than Bayer.
	VIDataTypeYUV uint32 = 0
	// VIWorkMode1Multiplex is one sensor on the device, not time-shared.
	VIWorkMode1Multiplex uint32 = 0
	// VIScanProgressive: HDMI is never interlaced here.
	VIScanProgressive uint32 = 1
	// VIPipeBypassNone runs the full pipe. The ISP stages are no-ops on a
	// YUV input but the pipe still has to exist to reach the channel.
	VIPipeBypassNone uint32 = 0

	// PixelFormatNV21 is the semi-planar 4:2:0 layout the encoder consumes.
	PixelFormatNV21 uint32 = 19
	// PixelFormatNV12 is the other chroma order, for reference.
	PixelFormatNV12 uint32 = 18

	// InputModeMipi selects CSI-2 on the combo receiver.
	InputModeMipi uint32 = 0
	// Yuv422_8Bit is the CSI-2 data type the bridge transmits.
	Yuv422_8Bit uint32 = 3
	// MipiWdrModeNone: no wide-dynamic-range multiplexing.
	MipiWdrModeNone uint32 = 0
)

// Module ids from MOD_ID_E, needed to name the endpoints of a bind.
const (
	ModVPSS uint32 = 6
	ModVENC uint32 = 7
	ModVI   uint32 = 14
)

// --- MIPI receiver -----------------------------------------------------

// MipiRx is the CSI-2 receiver sitting between the HDMI bridge and VI.
type MipiRx struct{ f *os.File }

// OpenMipiRx opens the receiver device.
func OpenMipiRx() (*MipiRx, error) {
	f, err := openDev(MipiRxDev)
	if err != nil {
		return nil, err
	}
	return &MipiRx{f: f}, nil
}

func (m *MipiRx) Close() error {
	if m.f == nil {
		return nil
	}
	err := m.f.Close()
	m.f = nil
	return err
}

// SetDevAttr configures the receiver for the signal the bridge is sending.
func (m *MipiRx) SetDevAttr(attr *ComboDevAttr) error {
	req := ioc(iocWrite, 'm', 0x01, unsafe.Sizeof(ComboDevAttr{}))
	if err := ioctl(m.f, req, unsafe.Pointer(attr)); err != nil {
		return fmt.Errorf("cvi: mipi set dev attr: %w", err)
	}
	return nil
}

// ResetSensor and friends drive the reset/clock lines the receiver owns. The
// HDMI bridge has its own clock, so only the reset sequence matters here: the
// receiver has to be taken out of reset after its attributes are set.
func (m *MipiRx) ResetSensor(dev uint32) error   { return m.devCmd(0x05, dev, "reset sensor") }
func (m *MipiRx) UnresetSensor(dev uint32) error { return m.devCmd(0x06, dev, "unreset sensor") }
func (m *MipiRx) ResetMipi(dev uint32) error     { return m.devCmd(0x07, dev, "reset mipi") }
func (m *MipiRx) EnableClock(dev uint32) error   { return m.devCmd(0x10, dev, "enable clock") }
func (m *MipiRx) DisableClock(dev uint32) error  { return m.devCmd(0x11, dev, "disable clock") }

func (m *MipiRx) devCmd(nr uintptr, dev uint32, what string) error {
	req := ioc(iocWrite, 'm', nr, unsafe.Sizeof(dev))
	if err := ioctl(m.f, req, unsafe.Pointer(&dev)); err != nil {
		return fmt.Errorf("cvi: mipi %s: %w", what, err)
	}
	return nil
}

// LT6911ComboAttr builds the receiver configuration for the LT6911 HDMI
// bridge on this board.
//
// Lane mapping is the one value not derivable from the headers: it describes
// how the bridge's CSI lanes are physically routed to the SoC's pads, so it
// comes from the board, not the part. This is the straight-through order --
// clock on lane 0, data on 1..4, no PN swaps -- which is the common layout and
// the one to change first if the receiver reports ECC or CRC errors while the
// bridge itself reports a locked signal.
func LT6911ComboAttr(dev uint32) *ComboDevAttr {
	var attr ComboDevAttr

	attr.Input_mode = InputModeMipi
	attr.Devno = dev

	mipi := &attr.Mipi_attr
	mipi.Raw_data_type = Yuv422_8Bit
	mipi.Wdr_mode = MipiWdrModeNone

	// lane_id[0] is the clock lane; the rest are data. -1 marks an unused
	// lane, which is how a 2-lane routing would be spelled.
	mipi.Lane_id = [5]int16{0, 1, 2, 3, 4}
	mipi.Pn_swap = [5]uint8{0, 0, 0, 0, 0}

	return &attr
}

// --- VI ----------------------------------------------------------------

// VI is the video-input block: it takes the receiver's stream and produces
// frames in VB blocks.
//
// Its interface is a single pair of ioctls over two command spaces. Id names
// an ISP-side VI_IOCTL; SdkId names the VI_SDK_CTRL lifecycle command this
// package uses. The kernel dispatches on whichever the ioctl number selects,
// so the two never collide.
type VI struct{ f *os.File }

// VI SDK commands (enum VI_SDK_CTRL, vi_uapi.h).
const (
	viSDKSetDevAttr uint32 = iota
	viSDKGetDevAttr
	viSDKEnableDev
	viSDKDisableDev
	viSDKCreatePipe
	viSDKDestroyPipe
	viSDKSetPipeAttr
	viSDKGetPipeAttr
	viSDKStartPipe
	viSDKStopPipe
	viSDKSetChnAttr
	viSDKGetChnAttr
	viSDKEnableChn
	viSDKDisableChn
)

// OpenVI opens the video-input device.
func OpenVI() (*VI, error) {
	f, err := openDev(VIDev)
	if err != nil {
		return nil, err
	}
	return &VI{f: f}, nil
}

func (v *VI) Close() error {
	if v.f == nil {
		return nil
	}
	err := v.f.Close()
	v.f = nil
	return err
}

// viIOCTLSdkCtrl is VI_IOCTL_SDK_CTRL, the 49th member of enum VI_IOCTL
// (vi_uapi.h). It is the outer selector: vi_ext_control carries two command
// fields, and the driver dispatches on Id first, only reading Sdk_id once Id
// says this is an SDK call.
//
// Leaving Id at zero does not fail -- zero is VI_IOCTL_ONLINE, a real command --
// so the ioctl returns success having set the online-mode flag from whatever
// happened to be in the struct, and every SetDevAttr/CreatePipe/EnableChn is
// silently discarded. The symptom is a pipeline that builds without error and
// produces no frames, with VI's proc tables empty.
const viIOCTLSdkCtrl uint32 = 48

// sdk issues one VI_SDK_CTRL command against a dev/pipe/chn triple.
func (v *VI) sdk(id uint32, dev, pipe, chn int32, ptr unsafe.Pointer, val int32, what string) error {
	ctl := VIExtControl{
		Id:     viIOCTLSdkCtrl,
		Sdk_id: id,
		Sdk_cfg: VISdkCfg{
			Dev:  dev,
			Pipe: pipe,
			Chn:  chn,
			Ptr:  (*byte)(ptr),
			Val:  val,
		},
	}
	req := ioc(iocRead|iocWrite, 'V', 0x21, unsafe.Sizeof(VIExtControl{}))
	if err := ioctl(v.f, req, unsafe.Pointer(&ctl)); err != nil {
		return fmt.Errorf("cvi: vi %s: %w", what, err)
	}
	return nil
}

func (v *VI) SetDevAttr(dev int32, attr *VIDevAttr) error {
	return v.sdk(viSDKSetDevAttr, dev, 0, 0, unsafe.Pointer(attr), 0, "set dev attr")
}

func (v *VI) EnableDev(dev int32) error {
	return v.sdk(viSDKEnableDev, dev, 0, 0, nil, 0, "enable dev")
}

func (v *VI) DisableDev(dev int32) error {
	return v.sdk(viSDKDisableDev, dev, 0, 0, nil, 0, "disable dev")
}

func (v *VI) CreatePipe(pipe int32, attr *VIPipeAttr) error {
	return v.sdk(viSDKCreatePipe, 0, pipe, 0, unsafe.Pointer(attr), 0, "create pipe")
}

func (v *VI) DestroyPipe(pipe int32) error {
	return v.sdk(viSDKDestroyPipe, 0, pipe, 0, nil, 0, "destroy pipe")
}

func (v *VI) StartPipe(pipe int32) error {
	return v.sdk(viSDKStartPipe, 0, pipe, 0, nil, 0, "start pipe")
}

func (v *VI) StopPipe(pipe int32) error {
	return v.sdk(viSDKStopPipe, 0, pipe, 0, nil, 0, "stop pipe")
}

func (v *VI) SetChnAttr(pipe, chn int32, attr *VIChnAttr) error {
	return v.sdk(viSDKSetChnAttr, 0, pipe, chn, unsafe.Pointer(attr), 0, "set chn attr")
}

func (v *VI) EnableChn(pipe, chn int32) error {
	return v.sdk(viSDKEnableChn, 0, pipe, chn, nil, 0, "enable chn")
}

func (v *VI) DisableChn(pipe, chn int32) error {
	return v.sdk(viSDKDisableChn, 0, pipe, chn, nil, 0, "disable chn")
}

// --- VPSS --------------------------------------------------------------

// VPSS scales and converts between VI's output and what the encoder wants.
// It is not optional even when no scaling is needed: it is what turns the
// capture format into the NV21 the VPU reads.
type VPSS struct{ f *os.File }

// OpenVPSS opens the scaler device.
func OpenVPSS() (*VPSS, error) {
	f, err := openDev(VPSSDev)
	if err != nil {
		return nil, err
	}
	return &VPSS{f: f}, nil
}

func (p *VPSS) Close() error {
	if p.f == nil {
		return nil
	}
	err := p.f.Close()
	p.f = nil
	return err
}

func (p *VPSS) cmd(nr uintptr, dir uintptr, arg unsafe.Pointer, size uintptr, what string) error {
	req := ioc(dir, 'S', nr, size)
	if err := ioctl(p.f, req, arg); err != nil {
		return fmt.Errorf("cvi: vpss %s: %w", what, err)
	}
	return nil
}

func (p *VPSS) CreateGroup(grp int32, attr *VPSSGrpAttr) error {
	cfg := VPSSCreateGrpCfg{VpssGrp: grp, StGrpAttr: *attr}
	return p.cmd(0x00, iocWrite, unsafe.Pointer(&cfg), unsafe.Sizeof(cfg), "create group")
}

func (p *VPSS) DestroyGroup(grp int32) error {
	return p.cmd(0x01, iocWrite, unsafe.Pointer(&grp), unsafe.Sizeof(grp), "destroy group")
}

func (p *VPSS) StartGroup(grp int32) error {
	cfg := VPSSStartGrpCfg{VpssGrp: grp}
	return p.cmd(0x03, iocWrite, unsafe.Pointer(&cfg), unsafe.Sizeof(cfg), "start group")
}

func (p *VPSS) StopGroup(grp int32) error {
	return p.cmd(0x04, iocWrite, unsafe.Pointer(&grp), unsafe.Sizeof(grp), "stop group")
}

func (p *VPSS) SetChnAttr(grp, chn int32, attr *VPSSChnAttr) error {
	cfg := VPSSChnAttrCfg{VpssGrp: grp, VpssChn: chn, StChnAttr: *attr}
	return p.cmd(0x21, iocWrite, unsafe.Pointer(&cfg), unsafe.Sizeof(cfg), "set chn attr")
}

func (p *VPSS) EnableChn(grp, chn int32) error {
	cfg := VPSSEnChnCfg{VpssGrp: grp, VpssChn: chn}
	return p.cmd(0x23, iocWrite, unsafe.Pointer(&cfg), unsafe.Sizeof(cfg), "enable chn")
}

func (p *VPSS) DisableChn(grp, chn int32) error {
	cfg := VPSSEnChnCfg{VpssGrp: grp, VpssChn: chn}
	return p.cmd(0x24, iocWrite, unsafe.Pointer(&cfg), unsafe.Sizeof(cfg), "disable chn")
}

// --- sys: binding stages together --------------------------------------

// Sys is the module that wires stages to each other.
type Sys struct{ f *os.File }

// OpenSys opens the sys device.
func OpenSys() (*Sys, error) {
	f, err := openDev(SysDev)
	if err != nil {
		return nil, err
	}
	return &Sys{f: f}, nil
}

func (s *Sys) Close() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// Bind connects a source channel to a destination channel inside the kernel.
//
// This is the call that keeps captured frames out of userspace: once VI is
// bound to VPSS and VPSS to VENC, every frame moves between stages by handing
// over a VB block, and this process only ever reads the encoded bitstream off
// the end. On a 1 GHz single-core part that is the difference between a
// working console and one that spends all its time memcpy'ing.
func (s *Sys) Bind(src, dst MMFChn) error { return s.bind(src, dst, true) }

// Unbind disconnects a previously bound pair.
func (s *Sys) Unbind(src, dst MMFChn) error { return s.bind(src, dst, false) }

func (s *Sys) bind(src, dst MMFChn, on bool) error {
	cfg := SysBindCfg{Mmf_chn_src: src, Mmf_chn_dst: dst}
	if on {
		cfg.Is_bind = 1
	}

	// The request encodes sizeof(unsigned long long) rather than the struct:
	// the vendor's macro says so, and the driver reads the pointer out of
	// arg regardless.
	req := ioc(iocWrite, 'S', 0x09, unsafe.Sizeof(uint64(0)))
	if err := ioctl(s.f, req, unsafe.Pointer(&cfg)); err != nil {
		what := "bind"
		if !on {
			what = "unbind"
		}
		return fmt.Errorf("cvi: sys %s %d.%d.%d -> %d.%d.%d: %w", what,
			src.EnModId, src.S32DevId, src.S32ChnId,
			dst.EnModId, dst.S32DevId, dst.S32ChnId, err)
	}
	return nil
}

// Chn names one endpoint of a bind.
func Chn(mod uint32, dev, chn int32) MMFChn {
	return MMFChn{EnModId: mod, S32DevId: dev, S32ChnId: chn}
}
