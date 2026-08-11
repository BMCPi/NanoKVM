package cvi

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Base is a handle on /dev/cvi-base, which owns chip identity and the VB
// (video buffer) allocator.
//
// VB matters because the encoder reads frames by physical address out of the
// ION carveout: a frame assembled in ordinary Go memory is invisible to it.
// Anything feeding VENC from userspace -- a test pattern, a still to encode as
// JPEG -- has to come from a VB block. Frames that arrive over a kernel-side
// bind from VI or VPSS are already VB blocks and need none of this.
type Base struct {
	f *os.File
}

// OpenBase opens the base device.
func OpenBase() (*Base, error) {
	f, err := openDev(BaseDev)
	if err != nil {
		return nil, err
	}
	return &Base{f: f}, nil
}

// Close releases the handle. It does not tear down VB; call VBExit first if
// this process owns the pools.
func (b *Base) Close() error {
	if b.f == nil {
		return nil
	}
	err := b.f.Close()
	b.f = nil
	return err
}

// Base device ioctls (cvi_base.h). The magic is 's' and these are the only
// ones this package needs.
var (
	baseReadChipID      = uintptr(0x80047301) // _IOR('s', 1, unsigned int)
	baseReadChipVersion = uintptr(0x80047302) // _IOR('s', 2, unsigned int)
	baseVBCmd           = uintptr(0xc0107308) // _IOWR('s', 8, struct vb_ext_control)
)

// VB command ids (enum VB_IOCTL, vb_uapi.h). They travel in
// vbExtControl.ID rather than in the ioctl number.
const (
	vbSetConfig uint32 = iota
	vbGetConfig
	vbInit
	vbExit
	vbCreatePool
	vbCreateExPool
	vbDestroyPool
	vbPhysToHandle
	vbGetBlkInfo
	vbGetPoolCfg
	vbGetBlock
	vbReleaseBlock
	vbGetPoolMaxCnt
	vbPrintPool
	vbUnitTest
	vbGetVBInit
)

// vbExtControl mirrors struct vb_ext_control, with the union typed as the
// 64-bit field every VB command actually uses: a pointer to the command's own
// struct, or a block handle for RELEASE_BLOCK.
//
// The generated VBExtControl renders that union as its first member (an int32
// plus padding), which cannot hold a pointer -- so this declares the same
// layout with a usable union field. The assertion below is what keeps the two
// from drifting: it fails to compile if the sizes ever differ.
type vbExtControl struct {
	ID       uint32
	Reserved uint32
	Value    uint64
}

var _ [unsafe.Sizeof(VBExtControl{})]byte = [unsafe.Sizeof(vbExtControl{})]byte{}

// ChipID reads the SoC identity register. It is the cheapest proof that the
// base driver is bound and its ioctl path works.
func (b *Base) ChipID() (uint32, error) {
	var id uint32
	if err := ioctl(b.f, baseReadChipID, unsafe.Pointer(&id)); err != nil {
		return 0, fmt.Errorf("cvi: read chip id: %w", err)
	}
	return id, nil
}

// ChipVersion reads the SoC revision register.
func (b *Base) ChipVersion() (uint32, error) {
	var v uint32
	if err := ioctl(b.f, baseReadChipVersion, unsafe.Pointer(&v)); err != nil {
		return 0, fmt.Errorf("cvi: read chip version: %w", err)
	}
	return v, nil
}

// vbCmd issues one VB command. ptr is the address the driver copies the
// command struct from and back to; pass 0 for commands that take no struct.
func (b *Base) vbCmd(id uint32, value uint64, what string) error {
	ctl := vbExtControl{ID: id, Value: value}
	if err := ioctl(b.f, baseVBCmd, unsafe.Pointer(&ctl)); err != nil {
		return fmt.Errorf("cvi: vb %s: %w", what, err)
	}
	return nil
}

// VBSetConfig declares the common pools. It only has an effect before VBInit;
// the driver ignores it once VB is up.
func (b *Base) VBSetConfig(cfg *VBCfg) error {
	return b.vbCmd(vbSetConfig, uint64(uintptr(unsafe.Pointer(cfg))), "set config")
}

// VBGetConfig reads the current pool configuration back.
func (b *Base) VBGetConfig() (*VBCfg, error) {
	var cfg VBCfg
	if err := b.vbCmd(vbGetConfig, uint64(uintptr(unsafe.Pointer(&cfg))), "get config"); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// VBInit brings the configured pools up, carving them out of the ION region.
func (b *Base) VBInit() error { return b.vbCmd(vbInit, 0, "init") }

// VBExit tears the pools down.
func (b *Base) VBExit() error { return b.vbCmd(vbExit, 0, "exit") }

// VBInited reports whether VB has been initialised, by this process or another.
func (b *Base) VBInited() (bool, error) {
	var inited int32
	err := b.vbCmd(vbGetVBInit, uint64(uintptr(unsafe.Pointer(&inited))), "get init state")
	if err != nil {
		return false, err
	}
	return inited != 0, nil
}

// VBGetBlock takes a block of at least size bytes from a pool. Pass
// VBPoolAny to let the allocator choose whichever common pool fits.
func (b *Base) VBGetBlock(poolID, size uint32) (uint64, error) {
	cfg := VBBlkCfg{Pool_id: poolID, Blk_size: size}
	if err := b.vbCmd(vbGetBlock, uint64(uintptr(unsafe.Pointer(&cfg))), "get block"); err != nil {
		return 0, err
	}
	return cfg.Blk, nil
}

// VBPoolAny asks the allocator to pick a common pool by block size rather than
// naming one. VB_INVALID_POOLID doubles as this sentinel in the vendor API.
const VBPoolAny uint32 = 0xffffffff

// VBReleaseBlock returns a block to its pool.
//
// The handle goes in the union directly rather than through a pointer -- this
// is the one VB command whose argument is a value, not a struct.
func (b *Base) VBReleaseBlock(blk uint64) error {
	return b.vbCmd(vbReleaseBlock, blk, "release block")
}

// VBBlockInfo resolves a block handle to its pool and physical address, which
// is what a frame descriptor has to carry for the VPU to find the pixels.
func (b *Base) VBBlockInfo(blk uint64) (*VBBlkInfo, error) {
	info := VBBlkInfo{Blk: blk}
	if err := b.vbCmd(vbGetBlkInfo, uint64(uintptr(unsafe.Pointer(&info))), "get block info"); err != nil {
		return nil, err
	}
	return &info, nil
}

// VBHandleForPhys is the reverse lookup, for a physical address that came from
// somewhere else in the pipeline.
func (b *Base) VBHandleForPhys(phys uint64) (uint64, error) {
	info := VBBlkInfo{Phy_addr: phys}
	if err := b.vbCmd(vbPhysToHandle, uint64(uintptr(unsafe.Pointer(&info))), "phys to handle"); err != nil {
		return 0, err
	}
	return info.Blk, nil
}

// ErrNoCPUMapping reports that a VB block cannot be mapped through the base
// device.
//
// Measured on hardware: base's mmap does not map the carveout at all. It
// remaps ndev->shared_mem -- a kzalloc'd BASE_SHARE_MEM_SIZE state page used
// for the log and sys proc interfaces -- and rejects any offset past it, so
// asking for a block's physical address returns EINVAL no matter how the call
// is spelled.
//
// This only blocks frames *originating* in userspace, which is a bring-up and
// snapshot concern. The streaming path never needs it: VI and VPSS fill VB
// blocks in kernel space and CVI_SYS_Bind hands them to VENC without the
// pixels ever crossing into this process. Getting a CPU mapping needs the ION
// device (/dev/ion, allocate then mmap the returned dma-buf) rather than base.
var ErrNoCPUMapping = fmt.Errorf(
	"cvi: base does not map VB memory; use /dev/ion for a CPU mapping")

// MapBlock is not implemented. See ErrNoCPUMapping for why, and for what to
// use instead. It exists so the gap is visible at the call site rather than
// being rediscovered as an unexplained EINVAL.
func (b *Base) MapBlock(phys uint64, size int) ([]byte, error) {
	return nil, ErrNoCPUMapping
}

// unused keeps the unix import while MapBlock is unimplemented; the streaming
// path will map through ION here.
var _ = unix.Munmap

// NV12Size is the byte size of an NV12 frame: full-resolution luma followed by
// a half-height interleaved chroma plane. It is the format the VI and VPSS
// hand to the encoder, so a userspace-supplied frame has to match.
func NV12Size(width, height int) int { return width * height * 3 / 2 }
