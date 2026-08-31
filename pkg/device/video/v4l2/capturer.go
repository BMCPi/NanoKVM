package v4l2

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/pi-bmc/nanokvm-app/pkg/device/video"
	"github.com/pi-bmc/nanokvm-app/pkg/device/video/lt6911"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

// Defaults applied where video.Config leaves a zero — the same values the
// cvi implementation shipped with, for the same reasons: a BMC console is
// nearly static, so a moderate frame rate and a long GOP are large wins on a
// single-core part, and the hub requests an IDR whenever a viewer joins.
const (
	defaultFPS     = 30
	defaultBitrate = 4_000_000
	defaultGOP     = 120

	// frameQueue is how many encoded frames may be in flight to the
	// consumer. Small on purpose: for live video a late frame is worth
	// less than a current one, so the right response to a slow consumer is
	// to drop rather than to buffer and add latency.
	frameQueue = 8

	// bufferCount is the vb2 buffer depth. The kernel feeder only pulls a
	// frame from the scaler when one of these is free, so this is also the
	// back-pressure depth; four keeps one in flight to the consumer, one
	// filling, and slack for jitter.
	bufferCount = 4

	// pollTimeout bounds every wait on the device so the loop can notice a
	// stop request or a config change even when the host sends nothing.
	pollTimeout = 500 // ms
)

// Capturer is the video.Capturer over /dev/videoN.
//
// One goroutine owns the device's streaming state end to end: it builds the
// stream when a signal is present, pumps frames, listens for source-change
// events, and rebuilds on mode switches. That single ownership is what the
// kernel driver's contract wants — STREAMOFF/ON is the rebuild primitive —
// and it means no lock is ever held across a blocking wait.
type Capturer struct {
	mu sync.Mutex

	log *slog.Logger

	fd   int
	bufs [][]byte

	cfg     video.Config
	started bool
	closed  bool

	// reconfig asks the run loop to rebuild the stream with the current
	// cfg/quality at its next wakeup (at most pollTimeout away).
	reconfig atomic.Bool

	ownedModules []string

	// frameBuf is the reusable delivery buffer. Frame.Data aliases it and
	// stays valid only until the next receive, per the interface contract;
	// that keeps the hot path at one copy (kernel buffer -> here) and zero
	// allocations.
	frameBuf []byte

	frames  chan video.Frame
	states  chan video.State
	dropped atomic.Uint64
	state   atomic.Pointer[video.State]
	quality atomic.Uint64 // float64 * 1e6

	stop chan struct{}
	wg   sync.WaitGroup
}

var _ video.Capturer = (*Capturer)(nil)

// Open loads the capture stack and acquires the video device.
//
// It does not require a connected host: a BMC boots with no cable attached
// as often as not, so a missing input signal is a state to report, never a
// reason to fail here. EDID provisioning happens after the device is found,
// through the kernel's VIDIOC_G/S_EDID — the bridge's i2c bus is entirely
// kernel-owned now, so userspace never touches /dev/i2c-4 at all.
func Open(log *slog.Logger) (*Capturer, error) {
	c := &Capturer{
		log:    logger.Or(log),
		fd:     -1,
		frames: make(chan video.Frame, frameQueue),
		states: make(chan video.State, 1),
		stop:   make(chan struct{}),
	}
	c.setQuality(1)
	c.publish(video.State{})

	var err error
	if c.ownedModules, err = loadPipelineModules(); err != nil {
		c.closeDevices()
		return nil, err
	}

	if c.fd, err = findDevice(); err != nil {
		c.closeDevices()
		return nil, err
	}

	// Give the bridge an EDID if it has none, through the kernel path
	// (VIDIOC_G/S_EDID on the capture node): the host picks its output
	// mode from what the bridge advertises, and the part ships with its
	// storage erased. Read-first, because programming is a flash
	// erase/write cycle and endurance is finite. Degraded-mode on
	// failure, never fatal — a bridge with no EDID still locks whatever
	// the host decides to send.
	if cur, err := c.EDID(); err != nil {
		c.log.Warn("v4l2: bridge EDID check skipped", slog.Any("err", err))
	} else if !lt6911.ValidEDID(cur) {
		if err := c.SetEDID(lt6911.DefaultEDID()); err != nil {
			c.log.Warn("v4l2: bridge EDID programming failed", slog.Any("err", err))
		} else {
			c.log.Info("v4l2: bridge EDID was blank or invalid, programmed the default")
		}
	}

	// Subscribe once for the life of the fd; V4L2 event subscriptions
	// survive STREAMOFF, so rebuilds never miss a mode change.
	sub := eventSubscription{Type: eventSourceChange}
	if err := vioctl(c.fd, reqSubscribeEvent(), unsafe.Pointer(&sub),
		"subscribe source-change"); err != nil {
		c.closeDevices()
		return nil, err
	}

	return c, nil
}

// findDevice locates the soph_v4l2 capture node. The number is not assumed:
// video device minors are allocated in probe order, and another V4L2 device
// (a UVC gadget, say) may exist. The brief retry covers the window between
// finit_module returning and devtmpfs exposing the node.
func findDevice() (int, error) {
	for attempt := 0; attempt < 20; attempt++ {
		for i := 0; i < 10; i++ {
			path := fmt.Sprintf("/dev/video%d", i)
			// Non-blocking, and readiness comes from poll: with a
			// blocking fd, DQEVENT would sleep until an event arrives,
			// which would wedge drainEvents on an empty queue.
			fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
			if err != nil {
				continue
			}
			var caps capability
			if err := vioctl(fd, reqQuerycap(), unsafe.Pointer(&caps),
				"querycap"); err == nil &&
				unix.ByteSliceToString(caps.Driver[:]) == "soph_v4l2" {
				return fd, nil
			}
			unix.Close(fd)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return -1, fmt.Errorf("v4l2: no soph_v4l2 video device found: %w",
		video.ErrNotSupported)
}

// Start begins supervising the stream. It is idempotent.
func (c *Capturer) Start(cfg video.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return errors.New("v4l2: Start on a closed capturer")
	}
	c.cfg = applyDefaults(cfg)
	if c.started {
		return nil
	}
	c.started = true

	c.wg.Add(1)
	go c.run()
	return nil
}

// Stop halts streaming, leaving the device open so Start can resume.
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
	c.stop = make(chan struct{})
	c.publish(video.State{})
	return nil
}

// Close tears everything down and releases the device and modules.
func (c *Capturer) Close() error {
	err := c.Stop()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeDevices()
	return err
}

func (c *Capturer) closeDevices() {
	if c.fd >= 0 {
		unix.Close(c.fd)
		c.fd = -1
	}
	// Modules last, once the descriptor into them is closed —
	// delete_module fails on a module still in use.
	if len(c.ownedModules) > 0 {
		if err := unloadPipelineModules(c.ownedModules); err != nil {
			c.log.Warn("v4l2: module unload failed", slog.Any("err", err))
		}
		c.ownedModules = nil
	}
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

func fourcc(c video.Codec) (uint32, error) {
	switch c {
	case video.CodecH264:
		return pixFmtH264, nil
	case video.CodecH265:
		return pixFmtHEVC, nil
	case video.CodecMJPEG:
		return pixFmtMJPEG, nil
	default:
		return 0, fmt.Errorf("v4l2: unsupported codec %v", c)
	}
}

// u32/i32 clamp bounded configuration values (sizes, rates, descriptors)
// into the ABI's fixed-width fields. Nothing legitimate here approaches the
// limits; the clamp exists so a corrupt value cannot wrap.
func u32(v int) uint32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return uint32(v)
}

func i32(v int) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

/* ------------------------------------------------------------------ */
/* The run loop                                                        */

func (c *Capturer) stopped() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

func (c *Capturer) run() {
	defer c.wg.Done()

	streaming := false
	defer func() {
		if streaming {
			c.stopStream()
		}
	}()

	for {
		if c.stopped() {
			return
		}

		if c.reconfig.Swap(false) && streaming {
			c.stopStream()
			streaming = false
		}

		if !streaming {
			switch err := c.startStream(); {
			case err == nil:
				streaming = true
			case errors.Is(err, unix.ENOLINK):
				// No input signal: a state, not an error. The kernel's
				// bridge poller emits a source-change event when a
				// signal appears, so wait on that rather than spinning —
				// and publish only the transition, not every idle pass
				// (observed on the board as an identical state twice a
				// second, waking every consumer for nothing).
				if c.State() != (video.State{}) {
					c.publish(video.State{})
				}
				c.waitEvent()
				continue
			default:
				c.publishErr(err)
				c.waitEvent()
				continue
			}
		}

		revents, err := c.poll(unix.POLLIN | unix.POLLPRI)
		if err != nil {
			c.publishErr(err)
			c.stopStream()
			streaming = false
			continue
		}

		if revents&unix.POLLPRI != 0 && c.drainEvents() {
			// The input mode changed (or the lock flipped). STREAMOFF /
			// STREAMON is the rebuild primitive: the kernel tears the
			// whole vendor pipeline down and rebuilds it against the new
			// timings.
			c.stopStream()
			streaming = false
			continue
		}

		if revents&(unix.POLLERR|unix.POLLHUP) != 0 {
			// vb2 marked the queue failed (the kernel feeder hit a fatal
			// pipeline error). Same rebuild cycle the cvi supervisor ran
			// on a dead frame loop.
			c.publishErr(errors.New("v4l2: stream errored, rebuilding"))
			c.stopStream()
			streaming = false
			continue
		}

		if revents&unix.POLLIN != 0 {
			if err := c.pumpOne(); err != nil {
				c.publishErr(err)
				c.stopStream()
				streaming = false
			}
		}
	}
}

// poll waits for device readiness or the timeout, whichever first.
func (c *Capturer) poll(events int16) (int16, error) {
	fds := []unix.PollFd{{Fd: i32(c.fd), Events: events}}
	for {
		n, err := unix.Poll(fds, pollTimeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("v4l2: poll: %w", err)
		}
		if n == 0 {
			return 0, nil
		}
		return fds[0].Revents, nil
	}
}

// waitEvent parks until a source-change event or the poll timeout. Called
// only while not streaming, so POLLIN cannot fire.
func (c *Capturer) waitEvent() {
	_, _ = c.poll(unix.POLLPRI)
	c.drainEvents()
}

// drainEvents empties the event queue, reporting whether anything that
// warrants a rebuild arrived.
func (c *Capturer) drainEvents() bool {
	rebuild := false
	for {
		var ev event
		if err := vioctl(c.fd, reqDqevent(), unsafe.Pointer(&ev),
			"dqevent"); err != nil {
			return rebuild
		}
		if ev.Type == eventSourceChange &&
			ev.srcChanges()&srcChangeResolution != 0 {
			rebuild = true
		}
	}
}

// startStream configures the device for the current input and config, maps
// buffers and starts streaming. ENOLINK means no input signal.
func (c *Capturer) startStream() error {
	var timings dvTimings
	if err := vioctl(c.fd, reqQueryDvTimings(), unsafe.Pointer(&timings),
		"query dv timings"); err != nil {
		return err
	}
	inW, inH := int(timings.width()), int(timings.height())

	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	pix, err := fourcc(cfg.Codec)
	if err != nil {
		return err
	}

	// An unset output size means "whatever the host is sending", which is
	// the right default for a KVM: the operator wants the console as it
	// is, not a rescaled approximation.
	outW, outH := cfg.Width, cfg.Height
	if outW == 0 || outH == 0 {
		outW, outH = inW, inH
	}
	outW &^= 1
	outH &^= 1

	var f format
	f.Type = bufTypeVideoCapture
	f.Pix.Width = u32(outW)
	f.Pix.Height = u32(outH)
	f.Pix.PixelFormat = pix
	if err := vioctl(c.fd, reqSFmt(), unsafe.Pointer(&f), "set format"); err != nil {
		return err
	}

	var parm streamParm
	parm.Type = bufTypeVideoCapture
	parm.Capture.TimePerFrame = fract{
		Numerator:   1,
		Denominator: u32(cfg.FrameRate),
	}
	if err := vioctl(c.fd, reqSParm(), unsafe.Pointer(&parm),
		"set frame rate"); err != nil {
		return err
	}

	if err := c.setCtrl(cidBitrate, i32(c.effectiveBitrate(cfg.Bitrate)),
		"bitrate"); err != nil {
		return err
	}
	if err := c.setCtrl(cidGopSize, i32(cfg.GOP), "gop"); err != nil {
		return err
	}

	if err := c.mapBuffers(); err != nil {
		c.unmapBuffers()
		return err
	}

	streamType := int32(bufTypeVideoCapture)
	if err := vioctl(c.fd, reqStreamon(), unsafe.Pointer(&streamType),
		"streamon"); err != nil {
		c.unmapBuffers()
		return err
	}

	c.publish(video.State{
		Ready:          true,
		Streaming:      video.StreamingActive,
		Width:          inW,
		Height:         inH,
		FramePerSecond: float64(cfg.FrameRate),
	})
	return nil
}

func (c *Capturer) stopStream() {
	streamType := int32(bufTypeVideoCapture)
	_ = vioctl(c.fd, reqStreamoff(), unsafe.Pointer(&streamType), "streamoff")
	c.unmapBuffers()
}

func (c *Capturer) mapBuffers() error {
	req := requestBuffers{
		Count:  bufferCount,
		Type:   bufTypeVideoCapture,
		Memory: memoryMMAP,
	}
	if err := vioctl(c.fd, reqReqbufs(), unsafe.Pointer(&req),
		"request buffers"); err != nil {
		return err
	}

	c.bufs = make([][]byte, req.Count)
	for i := range c.bufs {
		b := buffer{
			Index: uint32(i), Type: bufTypeVideoCapture,
			Memory: memoryMMAP,
		}
		if err := vioctl(c.fd, reqQuerybuf(), unsafe.Pointer(&b),
			"query buffer"); err != nil {
			return err
		}
		m, err := unix.Mmap(c.fd, int64(b.M&0xFFFF_FFFF), int(b.Length),
			unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("v4l2: mmap buffer %d: %w", i, err)
		}
		c.bufs[i] = m
		if err := vioctl(c.fd, reqQbuf(), unsafe.Pointer(&b),
			"queue buffer"); err != nil {
			return err
		}
	}
	return nil
}

func (c *Capturer) unmapBuffers() {
	for i, m := range c.bufs {
		if m != nil {
			_ = unix.Munmap(m)
			c.bufs[i] = nil
		}
	}
	c.bufs = nil

	// Freeing the buffers (count 0) lets the next build size them for a
	// different format.
	req := requestBuffers{Type: bufTypeVideoCapture, Memory: memoryMMAP}
	_ = vioctl(c.fd, reqReqbufs(), unsafe.Pointer(&req), "free buffers")
}

// pumpOne moves one encoded frame from the device to the consumer.
func (c *Capturer) pumpOne() error {
	b := buffer{Type: bufTypeVideoCapture, Memory: memoryMMAP}
	if err := vioctl(c.fd, reqDqbuf(), unsafe.Pointer(&b), "dqbuf"); err != nil {
		// The fd is non-blocking; a wakeup with nothing to dequeue is a
		// race, not a failure.
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		return err
	}

	n := int(b.BytesUsed)
	if int(b.Index) < len(c.bufs) && n > 0 && n <= len(c.bufs[b.Index]) {
		if cap(c.frameBuf) < n {
			c.frameBuf = make([]byte, n, n+n/2)
		}
		c.frameBuf = c.frameBuf[:n]
		copy(c.frameBuf, c.bufs[b.Index][:n])

		frame := video.Frame{
			Data: c.frameBuf,
			PTS: time.Duration(b.Timestamp.Sec)*time.Second +
				time.Duration(b.Timestamp.Usec)*time.Microsecond,
			Keyframe: b.Flags&bufFlagKeyframe != 0,
		}
		// Lossy by design: for live video a late frame is worth less than
		// a current one, so a slow consumer costs drops, not latency.
		select {
		case c.frames <- frame:
		default:
			c.dropped.Add(1)
		}
	}

	return vioctl(c.fd, reqQbuf(), unsafe.Pointer(&b), "requeue buffer")
}

func (c *Capturer) setCtrl(id uint32, value int32, what string) error {
	ctl := control{ID: id, Value: value}
	return vioctl(c.fd, reqSCtrl(), unsafe.Pointer(&ctl), "set "+what)
}

// effectiveBitrate scales the configured bitrate by the quality factor,
// with a floor so quality 0 still produces a usable stream.
func (c *Capturer) effectiveBitrate(bitrate int) int {
	f := c.QualityFactor()
	return int(float64(bitrate) * (0.1 + 0.9*f))
}

/* ------------------------------------------------------------------ */
/* State plumbing (latest-wins, same shape as the cvi implementation)  */

func (c *Capturer) publishErr(err error) {
	// Logged as well as published: the published form reaches the UI as
	// "not streaming", which looks the same as an idle host.
	c.log.Error("v4l2: pipeline error", slog.Any("err", err))

	st := c.State()
	st.Err = err.Error()
	st.Streaming = video.StreamingInactive
	c.publish(st)
}

func (c *Capturer) publish(s video.State) {
	c.state.Store(&s)
	for {
		select {
		case c.states <- s:
			return
		default:
			select {
			case <-c.states:
			default:
				return
			}
		}
	}
}

func (c *Capturer) State() video.State {
	if s := c.state.Load(); s != nil {
		return *s
	}
	return video.State{}
}

func (c *Capturer) States() <-chan video.State { return c.states }
func (c *Capturer) Frames() <-chan video.Frame { return c.frames }
func (c *Capturer) DroppedFrames() uint64      { return c.dropped.Load() }

// RequestKeyframe forces the next frame to be an IDR. Safe to call at any
// time: the kernel control is a no-op when nothing is encoding, and the
// next bring-up starts on an IDR anyway.
func (c *Capturer) RequestKeyframe() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.fd < 0 {
		return nil
	}
	return c.setCtrl(cidForceKeyframe, 1, "force keyframe")
}

func (c *Capturer) setQuality(f float64) {
	c.quality.Store(uint64(f * 1e6))
}

// SetQualityFactor scales the rate controller between 0 and 1, applied by
// rebuilding the stream: the encoder cannot re-negotiate rate control live,
// and a KVM changes quality about as often as someone drags a slider.
func (c *Capturer) SetQualityFactor(f float64) error {
	if f < 0 || f > 1 {
		return fmt.Errorf("v4l2: quality factor %v out of range [0,1]", f)
	}
	c.setQuality(f)
	c.reconfig.Store(true)
	return nil
}

func (c *Capturer) QualityFactor() float64 {
	return float64(c.quality.Load()) / 1e6
}

// SetCodec switches the encoder core, rebuilding the stream.
func (c *Capturer) SetCodec(codec video.Codec) error {
	if _, err := fourcc(codec); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg.Codec == codec {
		return nil
	}
	c.cfg.Codec = codec
	c.reconfig.Store(true)
	return nil
}

func (c *Capturer) Codec() video.Codec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.Codec
}

// EDID returns the raw EDID the bridge presents to the host, read from its
// flash through the kernel's VIDIOC_G_EDID (which serializes against the
// driver's own bridge traffic — the reason this lives behind the kernel).
func (c *Capturer) EDID() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.fd < 0 {
		return nil, fmt.Errorf("v4l2: EDID read: %w", video.ErrNotSupported)
	}
	buf := make([]byte, 256)
	e := v4l2Edid{Blocks: 2, Edid: &buf[0]}
	if err := vioctl(c.fd, reqGEdid(), unsafe.Pointer(&e), "get edid"); err != nil {
		return nil, err
	}
	return buf[:e.Blocks*128], nil
}

// SetEDID programs the bridge's EDID flash through VIDIOC_S_EDID. The host
// generally has to re-detect the display for it to take effect. Slow (the
// flash erase alone is half a second); not for hot paths.
func (c *Capturer) SetEDID(edid []byte) error {
	if !lt6911.ValidEDID(edid) {
		return fmt.Errorf("v4l2: refusing to write a malformed EDID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.fd < 0 {
		return fmt.Errorf("v4l2: EDID write: %w", video.ErrNotSupported)
	}
	e := v4l2Edid{Blocks: 2, Edid: &edid[0]}
	return vioctl(c.fd, reqSEdid(), unsafe.Pointer(&e), "set edid")
}
