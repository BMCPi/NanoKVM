package cvi

import (
	"fmt"
	"unsafe"
)

// VI's internal DMA working memory.
//
// The VI driver does not allocate the buffers its preraw/postraw DMA engines
// write into. It carves them out of a pool whose base and size come from
// userspace, and every internal allocation goes through _mempool_pop():
//
//	if ((isp_mempool.byteused + size) > isp_mempool.size) {
//	        vi_pr(VI_ERR, "reserved_memory(0x%x) is not enough...");
//	        return -EINVAL;
//	}
//
// With no pool set, isp_mempool.size is 0 and every one of those fails. The
// failure is quiet from userspace: the device still configures, still reports
// DevEn, still accepts START_STREAMING, and never raises a frame interrupt,
// because its DMA was never given anywhere to write. /proc/cvitek/vi shows
// IntCnt stuck at 0 and the driver prints "VI total reserved memory(0x0)".
//
// The vendor does this from its ISP layer, which is why it is easy to miss
// when the ISP is not being used: CVI_ISP_MemInit asks the driver how much it
// needs, allocates that from ION, and hands the physical address back.
const (
	// viIOCTLGetBufSize and viIOCTLSetDMABufInfo are VI_IOCTL_GET_BUF_SIZE and
	// VI_IOCTL_SET_DMA_BUF_INFO, members 41 and 42 of enum VI_IOCTL. The
	// numbering is confirmed by VI_IOCTL_SDK_CTRL landing on 48 in the same
	// enum, which is the value this package already uses successfully.
	viIOCTLGetBufSize    uint32 = 40
	viIOCTLSetDMABufInfo uint32 = 41

	// ionBufName is what the allocation shows up as in /proc's ION listing.
	ionBufName = "VI_ISP_DMA"
)

// VIDMABufInfo is struct cvi_vi_dma_buf_info (vi_isp.h).
type VIDMABufInfo struct {
	Paddr uint64
	Size  uint32
}

// SysIonData is struct sys_ion_data (sys_uapi.h). The explicit pad is the
// alignment C inserts before the 64-bit addr_p; the struct is not packed, so
// leaving it out would put addr_p four bytes early and return a torn address.
type SysIonData struct {
	Size     uint32
	Cached   uint32
	DmabufFd uint32
	_        uint32
	AddrP    uint64
	Name     [32]byte
}

// ctl issues a plain (non-SDK) VI ioctl.
//
// The control's trailing union is a value or a pointer depending on the
// command, and cgo renders it as an int32 plus padding. Writing a pointer
// therefore means writing all eight bytes, not just the int32 half.
func (v *VI) ctl(id uint32, ptr unsafe.Pointer, value int32) (int32, error) {
	ctl := VIExtControl{Id: id, Value: value}
	if ptr != nil {
		*(*uint64)(unsafe.Pointer(&ctl.Value)) = uint64(uintptr(ptr))
	}
	req := ioc(iocRead|iocWrite, 'V', 0x21, unsafe.Sizeof(VIExtControl{}))
	if err := ioctl(v.f, req, unsafe.Pointer(&ctl)); err != nil {
		return 0, err
	}
	return ctl.Value, nil
}

// ISPBufSize asks the driver how much DMA working memory it needs.
//
// The answer depends on the configured scene -- the driver dry-runs its own
// allocation against the current pipe setup and reports the total -- so this
// has to be called after the device and pipe attributes are set, not before.
func (v *VI) ISPBufSize() (uint32, error) {
	size, err := v.ctl(viIOCTLGetBufSize, nil, 0)
	if err != nil {
		return 0, fmt.Errorf("cvi: vi get isp buf size: %w", err)
	}
	if size <= 0 {
		return 0, fmt.Errorf("cvi: vi reported isp buf size %d", size)
	}
	return uint32(size), nil
}

// SetISPDMABuf gives the driver the pool its DMA engines write into.
func (v *VI) SetISPDMABuf(paddr uint64, size uint32) error {
	info := VIDMABufInfo{Paddr: paddr, Size: size}
	if _, err := v.ctl(viIOCTLSetDMABufInfo, unsafe.Pointer(&info), 0); err != nil {
		return fmt.Errorf("cvi: vi set isp dma buf: %w", err)
	}
	return nil
}

// IonAlloc allocates physically contiguous memory from the ION carveout.
//
// Uncached: this memory is only ever touched by the VI DMA engines, never by
// this process, so a cached mapping would buy nothing and cost coherency
// maintenance.
func (s *Sys) IonAlloc(size uint32, name string) (paddr uint64, fd int32, err error) {
	data := SysIonData{Size: size}
	copy(data.Name[:len(data.Name)-1], name)

	req := ioc(iocRead|iocWrite, 'S', 0x01, 8)
	if err := ioctl(s.f, req, unsafe.Pointer(&data)); err != nil {
		return 0, 0, fmt.Errorf("cvi: ion alloc %d bytes: %w", size, err)
	}
	if data.AddrP == 0 {
		return 0, 0, fmt.Errorf("cvi: ion alloc %d bytes returned a null address", size)
	}
	return data.AddrP, int32(data.DmabufFd), nil
}

// IonFree releases a buffer from IonAlloc. The driver keys on the physical
// address, not the descriptor.
func (s *Sys) IonFree(paddr uint64) error {
	data := SysIonData{AddrP: paddr}
	req := ioc(iocWrite, 'S', 0x02, 8)
	if err := ioctl(s.f, req, unsafe.Pointer(&data)); err != nil {
		return fmt.Errorf("cvi: ion free 0x%x: %w", paddr, err)
	}
	return nil
}

// setupISPMem sizes, allocates and installs VI's DMA pool. It must run after
// the pipe attributes are set and before the pipe is started.
func (c *Capturer) setupISPMem() error {
	size, err := c.vi.ISPBufSize()
	if err != nil {
		return err
	}

	paddr, _, err := c.sys.IonAlloc(size, ionBufName)
	if err != nil {
		return err
	}
	c.ispBufPaddr = paddr
	c.ispBufSize = size

	return c.vi.SetISPDMABuf(paddr, size)
}

// releaseISPMem returns the DMA pool to ION.
func (c *Capturer) releaseISPMem() error {
	if c.ispBufPaddr == 0 {
		return nil
	}
	err := c.sys.IonFree(c.ispBufPaddr)
	c.ispBufPaddr, c.ispBufSize = 0, 0
	return err
}
