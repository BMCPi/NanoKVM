// Package rtc streams the BMC's encoded HDMI capture to browsers over WebRTC.
//
// The pipeline hands out frames that are already H.264 (or H.265) access units
// straight from the SoC's encoder, so nothing here transcodes: a Hub owns one
// pump goroutine that moves frames from a video.Capturer onto a single shared
// track, and every browser Session binds to that same track. WebRTC is the
// transport of choice for a KVM because it is the only one browsers implement
// with real low-latency congestion control -- MJPEG-over-HTTP and WebSocket
// relays both buy simplicity with a second of lag.
//
// # Why one track for every viewer
//
// video.Capturer.Frames() is a single channel, so exactly one goroutine may
// consume it; a per-session pump would have sessions stealing frames from each
// other. pion's TrackLocalStaticSample can be added to any number of peer
// connections and fans each sample out to all of them, so one pump feeding one
// track is both the only correct shape and the cheapest: the encoder runs once
// no matter how many operators are watching.
//
// The cost is that WriteSample is synchronous, so a stalled client backs
// pressure up into the pump and the Capturer starts dropping frames for
// everyone. That is deliberate rather than overlooked -- Frames() is documented
// as lossy precisely so drops land there -- and the alternative, a buffered
// queue per session, means copying every frame out of the pipeline's buffers
// and reintroduces the latency WebRTC was chosen to avoid. Two operators on a
// LAN is the design point; this is not a fan-out video service.
package rtc

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
)

// ErrClosed reports use of a Hub that has been shut down.
var ErrClosed = errors.New("rtc: hub is closed")

const (
	// defaultFrameDuration is the sample duration assumed for the first
	// frame and whenever the measured interval is unusable. It advances the
	// RTP clock as if the source were 30 fps.
	defaultFrameDuration = time.Second / 30

	// maxFrameDuration bounds the measured interval. A gap longer than this
	// is a pipeline stall or a resumed stream, not a real frame interval,
	// and feeding it to the packetizer would jump the RTP timestamp far
	// enough to look like a stream restart to the receiver.
	maxFrameDuration = time.Second
)

// Options configures a Hub.
type Options struct {
	// ICEServers is the STUN/TURN list offered to the browser. It is empty
	// by default, and that default is deliberate: a BMC and its operator sit
	// on the same management network, where host candidates connect
	// directly. Pointing a management controller at a public STUN server
	// would announce its existence to a third party on every session, for no
	// benefit on the network it is actually deployed on. Set it only for a
	// genuinely routed deployment.
	ICEServers []webrtc.ICEServer

	// Capture is the pipeline configuration applied when the first viewer
	// connects. Its Codec field also fixes the track's codec for the life of
	// the Hub -- see Hub.Codec.
	Capture video.Config
}

// Hub owns the encoder-to-WebRTC path: one capture pipeline, one track, and
// the set of sessions bound to it.
//
// The pipeline runs only while at least one session is attached. On a BMC that
// matters: nothing is watching the console most of the time, and an idle
// encoder still burns power and VPU bandwidth.
type Hub struct {
	cap  video.Capturer
	opts Options
	api  *webrtc.API
	cfg  webrtc.Configuration

	track *webrtc.TrackLocalStaticSample

	mu       sync.Mutex
	sessions map[*Session]struct{}
	stop     chan struct{}
	wg       sync.WaitGroup
	closed   bool

	written atomic.Uint64
	state   atomic.Pointer[video.State]
}

// NewHub builds a Hub over cap. It does not start the pipeline; the first
// session to connect does that.
func NewHub(cap video.Capturer, opts Options) (*Hub, error) {
	if cap == nil {
		return nil, errors.New("rtc: nil capturer")
	}

	mime, err := mimeType(opts.Capture.Codec)
	if err != nil {
		return nil, err
	}

	// Register only the codec this Hub actually produces. Offering the
	// browser a menu it could pick VP8 from would let SDP negotiation
	// succeed and then deliver a stream nothing can decode, because there is
	// no software encoder behind this track to fall back to.
	m := &webrtc.MediaEngine{}
	if err := registerVideoCodec(m, mime); err != nil {
		return nil, err
	}

	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, fmt.Errorf("rtc: register interceptors: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: mime},
		"video", "nanokvm",
	)
	if err != nil {
		return nil, fmt.Errorf("rtc: create track: %w", err)
	}

	h := &Hub{
		cap:      cap,
		opts:     opts,
		api:      webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)),
		cfg:      webrtc.Configuration{ICEServers: opts.ICEServers},
		track:    track,
		sessions: make(map[*Session]struct{}),
	}
	s := cap.State()
	h.state.Store(&s)

	return h, nil
}

// Codec reports the codec this Hub's track carries.
//
// It is fixed for the Hub's lifetime. Switching codecs means a different track
// and a full renegotiation with every connected browser, and a BMC console has
// no reason to do that mid-session: H.264 is the only codec every browser
// decodes, so it is chosen once at startup. Callers that need a different one
// build a new Hub.
func (h *Hub) Codec() video.Codec { return h.opts.Capture.Codec }

// State returns the last capture state the Hub observed.
func (h *Hub) State() video.State { return *h.state.Load() }

// FramesWritten counts frames handed to the track. Paired with
// video.Capturer.DroppedFrames it says whether the encoder or the network is
// the bottleneck.
func (h *Hub) FramesWritten() uint64 { return h.written.Load() }

// Sessions reports how many browsers are currently attached.
func (h *Hub) Sessions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// Close tears down every session and stops the pipeline.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	sessions := make([]*Session, 0, len(h.sessions))
	for s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}

	h.mu.Lock()
	h.stopPipelineLocked()
	h.mu.Unlock()

	h.wg.Wait()
	return h.cap.Close()
}

// attach registers a session, starting the pipeline if it is the first.
func (h *Hub) attach(s *Session) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return ErrClosed
	}
	if len(h.sessions) == 0 {
		if err := h.startPipelineLocked(); err != nil {
			return err
		}
	}
	h.sessions[s] = struct{}{}
	return nil
}

// detach removes a session, stopping the pipeline if it was the last.
func (h *Hub) detach(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.sessions[s]; !ok {
		return
	}
	delete(h.sessions, s)
	if len(h.sessions) == 0 {
		h.stopPipelineLocked()
	}
}

func (h *Hub) startPipelineLocked() error {
	if err := h.cap.SetCodec(h.opts.Capture.Codec); err != nil {
		return fmt.Errorf("rtc: select codec: %w", err)
	}
	if err := h.cap.Start(h.opts.Capture); err != nil {
		return fmt.Errorf("rtc: start capture: %w", err)
	}

	h.stop = make(chan struct{})
	h.wg.Add(2)
	go h.pump(h.stop)
	go h.watchState(h.stop)
	return nil
}

func (h *Hub) stopPipelineLocked() {
	if h.stop == nil {
		return
	}
	close(h.stop)
	h.stop = nil
	if err := h.cap.Stop(); err != nil && !errors.Is(err, video.ErrNotSupported) {
		log.Warnf("rtc: stop capture: %s", err)
	}
}

// pump moves encoded frames from the pipeline onto the shared track.
func (h *Hub) pump(stop <-chan struct{}) {
	defer h.wg.Done()

	frames := h.cap.Frames()
	var prevPTS time.Duration
	havePrev := false

	for {
		select {
		case <-stop:
			return
		case f, ok := <-frames:
			if !ok {
				return
			}

			// pion advances the RTP clock by the sample's own Duration,
			// which is the interval to the *next* frame -- not knowable
			// until it arrives. Use the previous interval as the
			// estimate: it tracks the real frame rate without letting
			// error accumulate, and a receiver's jitter buffer absorbs
			// the one-frame lag. A first frame, a backwards PTS or a
			// stall longer than maxFrameDuration has no usable estimate,
			// so fall back rather than jump the timestamp.
			d := defaultFrameDuration
			if havePrev {
				if delta := f.PTS - prevPTS; delta > 0 && delta <= maxFrameDuration {
					d = delta
				}
			}
			prevPTS, havePrev = f.PTS, true

			// f.Data aliases the pipeline's buffer and is only valid
			// until the next receive. That is safe here because pion's
			// H.264 payloader copies each NAL into its own RTP payload
			// and WriteSample writes those synchronously, so nothing
			// retains the slice past this call. Any future path that
			// hands f.Data to another goroutine has to copy first.
			// A track with no bound sender discards the sample and
			// reports success, so frames produced between starting the
			// pipeline and the browser completing negotiation are simply
			// dropped rather than treated as failures.
			if err := h.track.WriteSample(media.Sample{Data: f.Data, Duration: d}); err != nil {
				log.Warnf("rtc: write sample: %s", err)
				continue
			}
			h.written.Add(1)
		}
	}
}

// watchState republishes capture state to every attached session, so the UI
// learns about resolution changes and unplugged cables without polling.
func (h *Hub) watchState(stop <-chan struct{}) {
	defer h.wg.Done()

	states := h.cap.States()
	for {
		select {
		case <-stop:
			return
		case st, ok := <-states:
			if !ok {
				return
			}
			h.state.Store(&st)

			h.mu.Lock()
			sessions := make([]*Session, 0, len(h.sessions))
			for s := range h.sessions {
				sessions = append(sessions, s)
			}
			h.mu.Unlock()

			for _, s := range sessions {
				s.sendState(st)
			}
		}
	}
}

// readRTCP drains receiver feedback for one sender.
//
// Draining is not optional: pion's interceptors buffer inbound RTCP per sender
// and stop processing once that buffer fills, which silently kills NACK and
// congestion feedback for the session. Reading it also gives us the one piece
// of feedback that needs acting on -- a PLI or FIR means the browser cannot
// decode what it has and needs a fresh IDR, which on a long-GOP console stream
// is the difference between recovering in milliseconds and staring at a frozen
// frame until the next scheduled keyframe.
func (h *Hub) readRTCP(sender *webrtc.RTPSender) {
	for {
		// Returns an error once the sender is stopped, which is how this
		// goroutine ends when the session closes.
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}

		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if err := h.cap.RequestKeyframe(); err != nil {
					log.Debugf("rtc: keyframe request: %s", err)
				}
			}
		}
	}
}

// mimeType maps a pipeline codec to the RTP MIME type browsers negotiate.
func mimeType(c video.Codec) (string, error) {
	switch c {
	case video.CodecH264:
		return webrtc.MimeTypeH264, nil
	case video.CodecH265:
		return webrtc.MimeTypeH265, nil
	default:
		// MJPEG has no standard WebRTC payload type; it is served by the
		// snapshot endpoint instead.
		return "", fmt.Errorf("rtc: codec %s cannot be carried over WebRTC", c)
	}
}

// registerVideoCodec adds one video codec to a bare MediaEngine.
//
// The SDP format lines matter as much as the MIME type. For H.264,
// packetization-mode=1 is what allows FU-A fragmentation of NALs larger than
// an MTU -- with mode 0 a single 1080p keyframe would be unsendable -- and
// constrained-baseline (profile-level-id 42e01f) is the profile every browser
// accepts without hardware-decoder caveats.
func registerVideoCodec(m *webrtc.MediaEngine, mime string) error {
	var fmtp string
	if mime == webrtc.MimeTypeH264 {
		fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
	}

	// Feedback the sender genuinely uses: NACK for retransmission, PLI/FIR
	// for the keyframe recovery readRTCP acts on, and REMB/TWCC so the
	// browser's congestion control has something to report against.
	fb := []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "ccm", Parameter: "fir"},
		{Type: "goog-remb"},
		{Type: webrtc.TypeRTCPFBTransportCC},
	}

	err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     mime,
			ClockRate:    90000,
			SDPFmtpLine:  fmtp,
			RTCPFeedback: fb,
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo)
	if err != nil {
		return fmt.Errorf("rtc: register codec %s: %w", mime, err)
	}
	return nil
}
