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
	"log"
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

	// streamPoll is the rate used once the pipeline is built. Reading the
	// bridge is not free the way a status register usually is: the read has
	// to open the part's register window, which stops the firmware driving
	// its CSI transmitter for the duration. Polling a live stream at
	// signalPoll measurably keeps the receiver from holding lock, so once
	// there is something to disturb, the rate drops.
	streamPoll = 3 * time.Second

	// getStreamTimeout bounds a blocking GetStream so the frame loop can
	// notice a shutdown request even while the host is sending nothing.
	getStreamTimeout = 500

	// getFrameTimeout bounds the two halves of the handoff from the scaler to
	// the encoder. Shorter than getStreamTimeout on purpose: at 30fps a frame
	// is due every 33ms, so waiting much longer than that for one means the
	// source has stopped rather than that the next frame is nearly ready, and
	// the loop is better off going back round to check whether it should
	// still be running.
	getFrameTimeout = 100

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
	txStarted      bool
	boundViVpss    bool
	pipeStarted    bool
	grpStarted     bool
	recvStarted    bool
	chnCreated     bool
	grpCreated     bool
	vpssChnEnabled bool
	pipeCreated    bool
	viChnEnabled   bool
	devEnabled     bool

	// VI's DMA working memory, held for as long as the pipeline is up. Zero
	// means nothing is allocated; see setupISPMem.
	ispBufPaddr uint64
	ispBufSize  uint32

	// The media modules this process inserted, in load order. Only these are
	// unloaded on the way out -- anything that was already there belongs to
	// whoever put it there.
	ownedModules []string

	// Puts the kernel console loglevel back as it was; never nil once Open
	// has returned. See quietKernelConsole.
	restoreConsole func()

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

	// Before the drivers, not after: they start reporting back-pressure the
	// moment a pipeline runs, and at 60 KERN_ERR a second that reporting is
	// itself capable of taking the board down. See quietKernelConsole.
	c.restoreConsole = quietKernelConsole()

	// The media drivers first: none of the devices below exists until they
	// are inserted, and nothing else on the system loads them.
	if c.ownedModules, err = loadMediaModules(); err != nil {
		return nil, err
	}

	if c.bridge, err = lt6911.Open(i2cDevice); err != nil {
		return nil, err
	}
	// Deliberately no Enable() here. The register window has to stay closed
	// except while a read is in flight, because holding it open takes the part
	// away from its own firmware and the CSI transmitter goes idle with it.
	// Signal brackets its own window; nothing else here needs one.

	// Give the bridge an EDID if it has none. The host picks its output mode
	// from what the bridge advertises, and this part ships from the factory
	// with its EDID storage erased, so without this the host is guessing.
	//
	// Not fatal: a bridge with no EDID still locks whatever the host decides
	// to send, so failing to program one is a degraded mode rather than a
	// reason to refuse to capture. It is also a flash erase/write, so it only
	// happens when what is stored is not a valid EDID.
	if wrote, edidErr := c.bridge.EnsureEDID(); edidErr != nil {
		log.Printf("cvi: bridge EDID: %v", edidErr)
	} else if wrote {
		log.Printf("cvi: bridge EDID was blank or invalid, programmed the default")
	}
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

	// Modules last, once every descriptor into them is closed -- delete_module
	// fails on a module that is still in use, and a device left open here
	// would be exactly that.
	if len(c.ownedModules) > 0 {
		if err := unloadMediaModules(c.ownedModules); err != nil {
			log.Printf("cvi: %v", err)
		}
		c.ownedModules = nil
	}

	// Last, once nothing is left that could still be spamming.
	if c.restoreConsole != nil {
		c.restoreConsole()
		c.restoreConsole = nil
	}
}

// supervise follows the input signal, building and rebuilding the pipeline to
// match it.
func (c *Capturer) supervise() {
	defer c.wg.Done()

	ticker := time.NewTicker(pollInterval())
	defer ticker.Stop()

	var frameLoop chan struct{}
	var frameWG sync.WaitGroup

	// Raised when the frame loop gives up. Buffered and never closed, because
	// it outlives any one loop: a new one is started on every rebuild.
	frameFailed := make(chan struct{}, 1)

	stopFrames := func() {
		if frameLoop == nil {
			return
		}
		close(frameLoop)
		frameWG.Wait()
		frameLoop = nil
		// Anything raised by the loop on its way out refers to a pipeline
		// that no longer exists.
		select {
		case <-frameFailed:
		default:
		}
	}
	defer stopFrames()

	startFrames := func() {
		if c.runW == 0 {
			return
		}
		frameLoop = make(chan struct{})
		frameWG.Add(1)
		go func(done chan struct{}) {
			defer frameWG.Done()
			c.runFrames(done, frameFailed)
		}(frameLoop)
	}

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
				startFrames()
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

		// Back off once the pipeline is up. Every poll opens the bridge's
		// register window, which halts the firmware driving its CSI
		// transmitter; at the idle rate that is often enough to keep the
		// receiver's lock from ever settling. Detecting an unplugged cable a
		// couple of seconds later is a much smaller cost than disturbing a
		// working stream twice a second.
		c.mu.Lock()
		built := c.runW != 0
		c.mu.Unlock()
		if built {
			ticker.Reset(streamPollInterval())
		} else {
			ticker.Reset(pollInterval())
		}

		select {
		case <-c.stop:
			return

		case <-frameFailed:
			// The frame loop is the only thing draining the encoder, so
			// losing it is not a degraded stream -- it is a pipeline with a
			// producer and no consumer. VENC's input queue fills, the VB
			// pool it is holding blocks from drains to nothing, and VI's
			// error handler starts resetting the ISP and retrying, one
			// KERN_ERR per frame. That is how a transient encoder error ends
			// up taking the whole board out.
			//
			// So tear the pipeline down. The next poll sees a locked signal
			// with nothing built and puts it back up, which is the same path
			// a mode change takes and is known to work.
			log.Printf("cvi: frame loop stopped, tearing the pipeline down")
			stopFrames()
			c.mu.Lock()
			_ = c.teardown()
			c.runW, c.runH = 0, 0
			c.mu.Unlock()

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
		// Log as well as publish. The published state reaches the UI as
		// "not ready", which is indistinguishable from having no cable
		// attached -- so without this a pipeline that fails every time
		// looks exactly like a host that is switched off.
		log.Printf("cvi: bring-up failed at %dx%d: %v", w, h, err)

		// Leave nothing half-built: a partially assembled pipeline is
		// what turns the next attempt's "already inited" into a
		// permanent failure.
		_ = c.teardown()
		c.publish(video.State{Err: err.Error()})
		return
	}
	log.Printf("cvi: pipeline up, %dx%d in, %dx%d out at %dfps", w, h, spec.outW, spec.outH, spec.fps)

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

	// Measure what the host is actually sending, so the VPSS channel can drop
	// the difference. This is the only reading in the whole bring-up that
	// costs the bridge a measurement window, which is why it happens here --
	// once per pipeline build -- rather than on the signal poll.
	//
	// A failure is not fatal: srcFPS falls back to the destination rate, which
	// makes the converter a no-op. That is the pre-existing behaviour, so a
	// bridge that will not answer leaves things no worse than they were.
	if fps, err := c.bridge.FrameRate(); err != nil {
		log.Printf("cvi: bridge frame rate: %v", err)
	} else if fps > 0 {
		s.inFPS = fps
		if fps > s.fps {
			log.Printf("cvi: source is %dfps, encoding at %d", fps, s.fps)
		}
	}
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
// runFrames drives the second half of the pipeline until done is closed.
//
// VPSS is not bound to the encoder (see bringUp), so this loop is what moves
// frames between them: collect a finished frame, hand it to the encoder, give
// the buffer back, then take whatever the encoder has finished. Running those
// in step is the point -- the loop can only go round as fast as the encoder
// lets it, so a host sending faster than the encoder can work simply leaves
// frames uncollected, and VPSS skips them instead of anything overflowing.
//
// It is also the only thing draining the encoder, so it must not stop quietly.
// Any exit that is not a shutdown raises failed, because a pipeline left
// running with nothing reading the far end of it backs up through VENC into
// the VB pool and takes the media stack down with it.
func (c *Capturer) runFrames(done <-chan struct{}, failed chan<- struct{}) {
	// Distinguishes "told to stop" from "gave up"; only the latter is a
	// reason to tear the pipeline down.
	stopped := false
	defer func() {
		if stopped {
			return
		}
		select {
		case failed <- struct{}{}:
		default: // already raised; one is enough
		}
	}()

	var stream VENCStream
	var frame VideoFrameInfo
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
			stopped = true
			return
		default:
		}

		// Collect a finished frame from the scaler. Coming up empty is
		// normal -- a console that is not changing still produces frames,
		// but a host that has gone quiet does not, and neither is a fault.
		if err := c.vpss.GetChnFrame(vpssGrp, vpssChn, &frame, getFrameTimeout); err != nil {
			if isNoFrame(err) {
				continue
			}
			c.publishErr(err)
			return
		}

		// Hand it to the encoder, then give the buffer back immediately.
		// Holding it across the encode would keep a VB block out of
		// circulation for the whole frame time for no reason -- the encoder
		// has taken what it needs by the time SendFrame returns.
		sendErr := c.enc.SendFrame(&frame, getFrameTimeout)
		if relErr := c.vpss.ReleaseChnFrame(vpssGrp, vpssChn, &frame); relErr != nil {
			// A frame that cannot be released is a block permanently gone
			// from the pool; a few of those and VI has nowhere to write.
			c.publishErr(relErr)
			return
		}
		if sendErr != nil {
			// The encoder refusing a frame is survivable -- it is the
			// back-pressure this design is built around -- so drop it and
			// carry on rather than tearing the pipeline down.
			if isNoFrame(sendErr) {
				continue
			}
			c.publishErr(sendErr)
			return
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
	// Logged as well as published, because the published form reaches the UI
	// as "not streaming", which looks the same as an idle host.
	log.Printf("cvi: %v", err)

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
