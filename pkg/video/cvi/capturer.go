// Package cvi implements the HDMI capture pipeline on the SG2002 (CV181x
// family) using the vendor's in-kernel multimedia drivers.
//
// The whole VI -> VPSS -> VENC path is bound inside the kernel, so captured
// frames never cross into this process as pixels: only the encoded bitstream
// is read out. That is what makes a KVM viable on a 1 GHz single-core part.
package cvi

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
	"github.com/pi-bmc/nanokvm-app/pkg/video/lt6911"
)

// Defaults applied where video.Config leaves a zero.
//
// The frame rate is deliberately below the 60 the bridge can deliver: a BMC
// console is nearly static, and halving the frame rate halves the encoder load
// on a part that has one core to spare for everything else the BMC does.
const (
	defaultFPS     = 30
	defaultBitrate = 4_000_000
	// A long GOP is a large win on a screen that barely changes. New
	// viewers do not wait for the next scheduled keyframe -- the hub asks
	// for an IDR when a session connects.
	defaultGOP = 120

	// frameQueue is how many encoded frames may be in flight to the
	// consumer. Small on purpose: for live video a late frame is worth less
	// than a current one, so the right response to a slow consumer is to
	// drop rather than to buffer and add latency.
	frameQueue = 8

	// signalPoll is how often the bridge is asked what it sees. Fast enough
	// that a resolution change or an unplugged cable shows up as a UI state
	// change promptly, slow enough to be free next to encoding.
	signalPoll = 500 * time.Millisecond

	// getStreamTimeout bounds a blocking GetStream so the frame loop can
	// notice a shutdown request even while the host is sending nothing.
	getStreamTimeout = 500

	// maxPacks bounds one frame's pack array. H.264 splits an access unit
	// into a handful of packs (SPS, PPS, slices); 16 is well clear of that
	// and the driver truncates to what it is given.
	maxPacks = 16
)

// Capturer is the video.Capturer for this SoC.
//
// Lifecycle is supervised rather than one-shot: the pipeline can only be
// configured once the input timing is known, and the host changes that at
// runtime (mode switches, power cycles, a cable pulled). So Start does not
// build the pipeline -- it starts a supervisor that builds it when a signal
// appears, tears it down when the signal goes away, and rebuilds it at the new
// size when the host switches modes.
type Capturer struct {
	mu sync.Mutex

	bridge *lt6911.Bridge

	base *Base
	mipi *MipiRx
	vi   *VI
	vpss *VPSS
	sys  *Sys
	enc  *Encoder
	bs   *bitstream

	// Stage-by-stage record of what has actually been brought up, so
	// teardown only unwinds what exists. A partial bring-up that failed
	// halfway is the common case here, not an edge case.
	boundViVpss    bool
	boundVpssVenc  bool
	pipeStarted    bool
	grpStarted     bool
	recvStarted    bool
	chnCreated     bool
	grpCreated     bool
	vpssChnEnabled bool
	pipeCreated    bool
	viChnEnabled   bool
	devEnabled     bool

	cfg     video.Config
	started bool
	closed  bool

	// running is the size the pipeline is currently built for; zero when it
	// is not built.
	runW, runH int

	frames  chan video.Frame
	states  chan video.State
	dropped atomic.Uint64

	state   atomic.Pointer[video.State]
	quality atomic.Uint64 // float64 bits

	stop chan struct{}
	wg   sync.WaitGroup
}

var _ video.Capturer = (*Capturer)(nil)

// Open acquires the capture devices and the HDMI bridge.
//
// It does not configure anything: a BMC boots with no host attached as often
// as not, and failing here would mean the video subsystem is unavailable for
// the rest of the process's life over a cable that gets plugged in a minute
// later.
func Open(i2cDevice string) (*Capturer, error) {
	c := &Capturer{
		frames: make(chan video.Frame, frameQueue),
		states: make(chan video.State, 1),
		stop:   make(chan struct{}),
	}
	c.setQuality(1)
	c.publish(video.State{})

	var err error
	defer func() {
		if err != nil {
			c.closeDevices()
		}
	}()

	if c.bridge, err = lt6911.Open(i2cDevice); err != nil {
		return nil, err
	}
	// Deliberately no Enable() here. The register window has to stay closed
	// except while a read is in flight, because holding it open takes the part
	// away from its own firmware and the CSI transmitter goes idle with it.
	// Signal brackets its own window; nothing else here needs one.
	if c.base, err = OpenBase(); err != nil {
		return nil, err
	}
	if c.mipi, err = OpenMipiRx(); err != nil {
		return nil, err
	}
	if c.vi, err = OpenVI(); err != nil {
		return nil, err
	}
	if c.vpss, err = OpenVPSS(); err != nil {
		return nil, err
	}
	if c.sys, err = OpenSys(); err != nil {
		return nil, err
	}
	if c.enc, err = OpenEncoder(vencChn); err != nil {
		return nil, err
	}
	if c.bs, err = openBitstream(); err != nil {
		return nil, err
	}
	return c, nil
}

// Start begins supervising the pipeline. It is idempotent.
func (c *Capturer) Start(cfg video.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return errors.New("cvi: Start on a closed capturer")
	}
	c.cfg = applyDefaults(cfg)
	if c.started {
		return nil
	}
	c.started = true

	c.wg.Add(1)
	go c.supervise()
	return nil
}

// Stop halts encoding and releases the pipeline, leaving the devices open so
// Start can bring it back up without reopening anything.
func (c *Capturer) Stop() error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil
	}
	c.started = false
	close(c.stop)
	c.mu.Unlock()

	c.wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.teardown()
	c.runW, c.runH = 0, 0
	c.stop = make(chan struct{})
	c.publish(video.State{})
	return err
}

// Close tears the pipeline down and releases every device.
func (c *Capturer) Close() error {
	err := c.Stop()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeDevices()
	return err
}

func (c *Capturer) closeDevices() {
	for _, closer := range []func() error{
		func() error {
			if c.bs != nil {
				return c.bs.Close()
			}
			return nil
		},
		func() error {
			if c.enc != nil {
				return c.enc.Close()
			}
			return nil
		},
		func() error {
			if c.sys != nil {
				return c.sys.Close()
			}
			return nil
		},
		func() error {
			if c.vpss != nil {
				return c.vpss.Close()
			}
			return nil
		},
		func() error {
			if c.vi != nil {
				return c.vi.Close()
			}
			return nil
		},
		func() error {
			if c.mipi != nil {
				return c.mipi.Close()
			}
			return nil
		},
		func() error {
			if c.base != nil {
				return c.base.Close()
			}
			return nil
		},
		func() error {
			if c.bridge != nil {
				return c.bridge.Close()
			}
			return nil
		},
	} {
		_ = closer()
	}
}

// supervise follows the input signal, building and rebuilding the pipeline to
// match it.
func (c *Capturer) supervise() {
	defer c.wg.Done()

	ticker := time.NewTicker(signalPoll)
	defer ticker.Stop()

	var frameLoop chan struct{}
	var frameWG sync.WaitGroup

	stopFrames := func() {
		if frameLoop == nil {
			return
		}
		close(frameLoop)
		frameWG.Wait()
		frameLoop = nil
	}
	defer stopFrames()

	for {
		sig, err := c.bridge.Signal()
		if err != nil {
			c.publish(video.State{Err: err.Error()})
		} else {
			c.mu.Lock()
			built := c.runW != 0
			sameSize := built && c.runW == sig.Width && c.runH == sig.Height
			c.mu.Unlock()

			switch {
			case sig.Locked && !sameSize:
				// A mode change means the pipeline is configured
				// for the wrong size; there is no reconfiguring
				// it in place, so it goes down and comes back.
				stopFrames()
				c.rebuild(sig.Width, sig.Height)
				c.mu.Lock()
				if c.runW != 0 {
					frameLoop = make(chan struct{})
					frameWG.Add(1)
					go func(done chan struct{}) {
						defer frameWG.Done()
						c.runFrames(done)
					}(frameLoop)
				}
				c.mu.Unlock()

			case !sig.Locked && built:
				// The host stopped sending. Release the encoder
				// rather than leaving it spinning on a dead
				// input -- and report it as a state, not an
				// error, because an unplugged cable is normal.
				stopFrames()
				c.mu.Lock()
				_ = c.teardown()
				c.runW, c.runH = 0, 0
				c.mu.Unlock()
				c.publish(video.State{})
			}
		}

		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
	}
}

// rebuild tears down whatever exists and brings the pipeline up for w*h.
func (c *Capturer) rebuild(w, h int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	_ = c.teardown()
	c.runW, c.runH = 0, 0

	spec := c.specLocked(w, h)
	if err := c.bringUp(spec); err != nil {
		// Leave nothing half-built: a partially assembled pipeline is
		// what turns the next attempt's "already inited" into a
		// permanent failure.
		_ = c.teardown()
		c.publish(video.State{Err: err.Error()})
		return
	}

	c.runW, c.runH = w, h
	c.publish(video.State{
		Ready:          true,
		Streaming:      video.StreamingActive,
		Width:          spec.outW,
		Height:         spec.outH,
		FramePerSecond: float64(spec.fps),
	})
}

// specLocked resolves the requested config against the input timing. Callers
// hold c.mu.
func (c *Capturer) specLocked(inW, inH int) pipeSpec {
	s := pipeSpec{
		inW:     inW,
		inH:     inH,
		outW:    c.cfg.Width,
		outH:    c.cfg.Height,
		fps:     c.cfg.FrameRate,
		bitrate: c.cfg.Bitrate,
		gop:     c.cfg.GOP,
		codec:   c.cfg.Codec,
	}
	// An unset output size means "whatever the host is sending", which is
	// the right default for a KVM: the operator wants to see the console as
	// it is, not a rescaled approximation of it.
	if s.outW == 0 || s.outH == 0 {
		s.outW, s.outH = inW, inH
	}
	// The encoder works in macroblocks; an odd size is rejected.
	s.outW &^= 1
	s.outH &^= 1
	return s
}

func applyDefaults(cfg video.Config) video.Config {
	if cfg.FrameRate <= 0 {
		cfg.FrameRate = defaultFPS
	}
	if cfg.Bitrate <= 0 {
		cfg.Bitrate = defaultBitrate
	}
	if cfg.GOP <= 0 {
		cfg.GOP = defaultGOP
	}
	return cfg
}

// runFrames pulls encoded frames until done is closed.
func (c *Capturer) runFrames(done <-chan struct{}) {
	var stream VENCStream
	packs := make([]VENCPack, maxPacks)

	// One scratch buffer reused for every frame. This is what Frame.Data
	// aliases, and why the documented contract is that it stays valid only
	// until the next receive: the alternative is an allocation per frame,
	// which at 30fps is exactly the sort of steady garbage a single-core
	// part cannot afford.
	buf := make([]byte, 0, 256<<10)

	for {
		select {
		case <-done:
			return
		default:
		}

		if err := c.enc.GetStream(&stream, packs, getStreamTimeout); err != nil {
			// No frame is normal on a static console; only report
			// something that looks like a real fault.
			if isNoFrame(err) {
				continue
			}
			c.publishErr(err)
			return
		}

		n := int(stream.U32PackCount)
		if n > len(packs) {
			n = len(packs)
		}

		buf = buf[:0]
		var pts uint64
		keyframe := false
		var readErr error
		for i := 0; i < n; i++ {
			p := &packs[i]
			if i == 0 {
				pts = p.U64PTS
			}
			if isKeyframePack(p) {
				keyframe = true
			}
			buf, readErr = c.bs.read(buf, p.U64PhyAddr+uint64(p.U32Offset), p.U32Len-p.U32Offset)
			if readErr != nil {
				break
			}
		}

		// Release before delivering: the frame lives in our scratch
		// buffer now, so holding the encoder's buffer across a channel
		// send would stall encoding on consumer speed for no reason.
		if err := c.enc.ReleaseStream(&stream, getStreamTimeout); err != nil {
			c.publishErr(err)
			return
		}
		if readErr != nil {
			c.publishErr(readErr)
			return
		}

		frame := video.Frame{
			Data:     buf,
			PTS:      time.Duration(pts) * time.Microsecond,
			Keyframe: keyframe,
		}

		select {
		case c.frames <- frame:
		default:
			// Lossy by design. A climbing count means the encoder is
			// outrunning the network or the WebRTC writer.
			c.dropped.Add(1)
		}
	}
}

// isKeyframePack reports whether a pack carries an IDR.
//
// DataType is a union of per-codec enums rendered as 4 bytes by the layout
// generator. For H.264 the IDR member is 5 (H264E_NALU_ISLICE); for H.265 it
// is 19 (H265E_NALU_IDRSLICE). Both are read from the first byte because the
// enum is little-endian and none of the values exceed 255.
func isKeyframePack(p *VENCPack) bool {
	switch p.DataType[0] {
	case 5, 19:
		return true
	}
	return false
}

// isNoFrame reports whether GetStream simply came up empty.
//
// These drivers have no ETIMEDOUT: CVI_VENC_GetStream returns CVI_ERR_VENC_BUSY
// when its bounded semaphore wait expires, and EMPTY_STREAM_FRAME / EMPTY_PACK /
// BUF_EMPTY when there is nothing queued. On a BMC console -- a screen that can
// sit unchanged for hours -- this is the overwhelmingly common outcome, so
// treating it as an error would take the pipeline down on an idle host.
func isNoFrame(err error) bool {
	return errors.Is(err, ErrBusy) ||
		errors.Is(err, ErrBufEmpty) ||
		errors.Is(err, ErrVencEmptyStreamFrame) ||
		errors.Is(err, ErrVencEmptyPack) ||
		errors.Is(err, ErrVencFrcNoEnc)
}

func (c *Capturer) publishErr(err error) {
	st := c.State()
	st.Err = err.Error()
	st.Streaming = video.StreamingInactive
	c.publish(st)
}

// publish records the state and pushes it, latest-wins.
func (c *Capturer) publish(s video.State) {
	c.state.Store(&s)
	for {
		select {
		case c.states <- s:
			return
		default:
			// Drop the stale one and retry, so a slow consumer
			// misses intermediates but always converges on current.
			select {
			case <-c.states:
			default:
				return
			}
		}
	}
}

// State returns the most recent known state.
func (c *Capturer) State() video.State {
	if s := c.state.Load(); s != nil {
		return *s
	}
	return video.State{}
}

func (c *Capturer) States() <-chan video.State { return c.states }
func (c *Capturer) Frames() <-chan video.Frame { return c.frames }
func (c *Capturer) DroppedFrames() uint64      { return c.dropped.Load() }

// RequestKeyframe forces the next frame to be an IDR.
func (c *Capturer) RequestKeyframe() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.chnCreated {
		return nil // nothing encoding yet; the next bring-up starts on an IDR
	}
	return c.enc.RequestIDR()
}

func (c *Capturer) setQuality(f float64) {
	c.quality.Store(uint64(f * 1e6))
}

// SetQualityFactor scales the rate controller between 0 and 1.
//
// It is applied by rebuilding the channel rather than by poking the live rate
// controller: SetRcParam takes a mode-specific struct this package does not
// yet render, and a KVM changes quality about as often as someone drags a
// slider, so the brief gap costs less than carrying that layout.
func (c *Capturer) SetQualityFactor(f float64) error {
	if f < 0 || f > 1 {
		return fmt.Errorf("cvi: quality factor %v out of range [0,1]", f)
	}
	c.setQuality(f)

	c.mu.Lock()
	w, h := c.runW, c.runH
	c.mu.Unlock()
	if w == 0 {
		return nil
	}
	c.rebuild(w, h)
	return nil
}

func (c *Capturer) QualityFactor() float64 {
	return float64(c.quality.Load()) / 1e6
}

// SetCodec switches the encoder core, restarting the pipeline.
func (c *Capturer) SetCodec(codec video.Codec) error {
	if _, _, err := codecParams(codec); err != nil {
		return err
	}

	c.mu.Lock()
	if c.cfg.Codec == codec {
		c.mu.Unlock()
		return nil
	}
	c.cfg.Codec = codec
	w, h := c.runW, c.runH
	c.mu.Unlock()

	if w == 0 {
		return nil
	}
	c.rebuild(w, h)
	return nil
}

func (c *Capturer) Codec() video.Codec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.Codec
}

// EDID returns the raw EDID presented to the host.
//
// Not implemented: the LT6911C takes its EDID over a separate register bank
// this package does not drive yet, and returning a fabricated block would be
// worse than saying so -- callers would present it as what the host sees.
func (c *Capturer) EDID() ([]byte, error) {
	return nil, fmt.Errorf("cvi: EDID read: %w", video.ErrNotSupported)
}

// SetEDID replaces the EDID presented to the host. See EDID.
func (c *Capturer) SetEDID([]byte) error {
	return fmt.Errorf("cvi: EDID write: %w", video.ErrNotSupported)
}
