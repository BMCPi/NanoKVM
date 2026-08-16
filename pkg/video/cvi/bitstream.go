package cvi

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// bitstream reads encoded frames out of the ION carveout.
//
// VENC hands userspace a VENCPack describing where a frame landed in physical
// memory. The pack's Pu8Addr is a *kernel* virtual address -- cvi_vc_drv.c
// copies the kernel-side packs straight out to the caller -- and the encoder
// chardev has no .mmap, so there is no route to those bytes through the driver
// that produced them. Base's mmap is no help either: it remaps a kzalloc'd
// state page, not the carveout (see ErrNoCPUMapping).
//
// That leaves /dev/mem, which is also what the vendor's own userspace does
// (CVI_SYS_Mmap). It works here because the kernel is built with
// CONFIG_DEVMEM=y and CONFIG_STRICT_DEVMEM unset; with STRICT_DEVMEM on, the
// carveout would be off limits and this would need an ION dma-buf instead.
type bitstream struct {
	mu      sync.Mutex
	f       *os.File
	windows map[uint64][]byte
}

// Windows are mapped in fixed aligned spans rather than per-pack.
//
// The encoder writes frames at moving offsets inside a bitstream ring, so
// caching by exact (start,end) would miss on nearly every frame. Fixed spans
// make the cache converge instead: the carveout is ~105 MiB, so the whole heap
// is at most a couple of dozen windows and in practice a stream touches two or
// three.
const (
	bsWindow = 4 << 20 // 4 MiB, a multiple of the page size
	bsMask   = ^uint64(bsWindow - 1)

	// maxWindows bounds the damage if a bad physical address ever arrives:
	// without it a corrupt pack could walk this process through an
	// unbounded amount of address space one 4 MiB mapping at a time.
	maxWindows = 64
)

// openBitstream opens /dev/mem for reading encoded frames.
//
// O_DSYNC is not incidental. drivers/char/mem.c decides cacheability in
// uncached_access(): a descriptor carrying O_DSYNC is mapped non-cached,
// anything else within known memory is mapped cached. The VPU writes the
// bitstream by DMA, so a cached mapping would let this process read whatever
// the CPU last had in its lines for those addresses -- stale frames, or a
// mixture of two. Since the pages are read exactly once each and never
// re-read, there is no cache benefit being given up.
func openBitstream() (*bitstream, error) {
	f, err := os.OpenFile("/dev/mem", os.O_RDONLY|unix.O_DSYNC, 0)
	if err != nil {
		return nil, fmt.Errorf("cvi: open /dev/mem for bitstream: %w", err)
	}
	return &bitstream{f: f, windows: make(map[uint64][]byte)}, nil
}

// window returns the mapping covering base, which must be window-aligned.
// Callers hold b.mu.
func (b *bitstream) window(base uint64) ([]byte, error) {
	if w, ok := b.windows[base]; ok {
		return w, nil
	}
	if len(b.windows) >= maxWindows {
		return nil, fmt.Errorf("cvi: bitstream: refusing to map more than %d windows "+
			"(physical address 0x%x outside the expected carveout?)", maxWindows, base)
	}

	w, err := unix.Mmap(int(b.f.Fd()), int64(base), bsWindow,
		unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("cvi: bitstream: map 0x%x+0x%x: %w", base, bsWindow, err)
	}
	b.windows[base] = w
	return w, nil
}

// read appends the len bytes at physical address phys to dst and returns the
// extended slice.
//
// A pack can straddle a window boundary, so this walks windows rather than
// assuming one covers the whole run.
func (b *bitstream) read(dst []byte, phys uint64, length uint32) ([]byte, error) {
	if length == 0 {
		return dst, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.f == nil {
		return dst, fmt.Errorf("cvi: bitstream: read from closed reader")
	}

	remaining := uint64(length)
	for remaining > 0 {
		base := phys & bsMask
		off := phys - base

		w, err := b.window(base)
		if err != nil {
			return dst, err
		}

		n := uint64(bsWindow) - off
		if n > remaining {
			n = remaining
		}
		dst = append(dst, w[off:off+n]...)

		phys += n
		remaining -= n
	}
	return dst, nil
}

// Close unmaps every window and releases /dev/mem.
func (b *bitstream) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for base, w := range b.windows {
		_ = unix.Munmap(w)
		delete(b.windows, base)
	}
	if b.f == nil {
		return nil
	}
	err := b.f.Close()
	b.f = nil
	return err
}
