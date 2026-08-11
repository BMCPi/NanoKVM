package rtc

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
)

// fakeCapturer is a video.Capturer whose frames the test writes by hand. It
// records the calls a Hub is expected to make so the lifecycle assertions can
// look at counts rather than at timing.
type fakeCapturer struct {
	frames chan video.Frame
	states chan video.State

	mu        sync.Mutex
	starts    int
	stops     int
	keyframes int
	codec     video.Codec
	startErr  error
}

func newFakeCapturer() *fakeCapturer {
	return &fakeCapturer{
		frames: make(chan video.Frame),
		states: make(chan video.State, 1),
	}
}

func (f *fakeCapturer) Start(cfg video.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.starts++
	return nil
}

func (f *fakeCapturer) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeCapturer) Close() error { return nil }

func (f *fakeCapturer) State() video.State {
	return video.State{Ready: true, Width: 1920, Height: 1080}
}

func (f *fakeCapturer) States() <-chan video.State { return f.states }
func (f *fakeCapturer) Frames() <-chan video.Frame { return f.frames }
func (f *fakeCapturer) DroppedFrames() uint64      { return 0 }

func (f *fakeCapturer) RequestKeyframe() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyframes++
	return nil
}

func (f *fakeCapturer) SetQualityFactor(float64) error { return nil }
func (f *fakeCapturer) QualityFactor() float64         { return 1 }

func (f *fakeCapturer) SetCodec(c video.Codec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codec = c
	return nil
}

func (f *fakeCapturer) Codec() video.Codec { return f.codec }

func (f *fakeCapturer) EDID() ([]byte, error) { return nil, nil }
func (f *fakeCapturer) SetEDID([]byte) error  { return nil }

func (f *fakeCapturer) counts() (starts, stops, keyframes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops, f.keyframes
}

// chanSignaler collects outbound signaling for the test to inspect or forward.
type chanSignaler struct {
	msgs chan Message
}

func newChanSignaler() *chanSignaler {
	// Buffered generously: the ICE agent emits candidates from its own
	// goroutine and must never block on a test that is not reading yet.
	return &chanSignaler{msgs: make(chan Message, 64)}
}

func (c *chanSignaler) Send(m Message) error {
	select {
	case c.msgs <- m:
	default:
	}
	return nil
}

func newTestHub(t *testing.T, cap video.Capturer) *Hub {
	t.Helper()
	hub, err := NewHub(cap, Options{Capture: video.Config{Codec: video.CodecH264}})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	return hub
}

func TestNewHubRejectsCodecWebRTCCannotCarry(t *testing.T) {
	_, err := NewHub(newFakeCapturer(), Options{Capture: video.Config{Codec: video.CodecMJPEG}})
	if err == nil {
		t.Fatal("expected MJPEG to be rejected: it has no WebRTC payload type")
	}
}

// TestPipelineRunsOnlyWhileWatched is the behaviour that keeps an idle BMC from
// encoding into the void: the first viewer starts the pipeline, the last one
// stops it, and viewers in between do neither.
func TestPipelineRunsOnlyWhileWatched(t *testing.T) {
	cap := newFakeCapturer()
	hub := newTestHub(t, cap)

	first, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if starts, stops, _ := cap.counts(); starts != 1 || stops != 0 {
		t.Fatalf("after first session: starts=%d stops=%d, want 1/0", starts, stops)
	}

	second, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	if starts, stops, _ := cap.counts(); starts != 1 || stops != 0 {
		t.Fatalf("second session restarted the pipeline: starts=%d stops=%d, want 1/0", starts, stops)
	}

	first.Close()
	if starts, stops, _ := cap.counts(); starts != 1 || stops != 0 {
		t.Fatalf("pipeline stopped while a viewer remained: starts=%d stops=%d, want 1/0", starts, stops)
	}

	second.Close()
	if starts, stops, _ := cap.counts(); starts != 1 || stops != 1 {
		t.Fatalf("after last session: starts=%d stops=%d, want 1/1", starts, stops)
	}

	// A later viewer starts it again rather than finding a dead hub.
	third, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("third session: %v", err)
	}
	if starts, _, _ := cap.counts(); starts != 2 {
		t.Fatalf("pipeline did not restart for a new viewer: starts=%d, want 2", starts)
	}
	third.Close()

	if err := hub.Close(); err != nil {
		t.Fatalf("hub close: %v", err)
	}
}

func TestSessionRefusedWhenPipelineWillNotStart(t *testing.T) {
	cap := newFakeCapturer()
	cap.startErr = errors.New("no capture hardware")
	hub := newTestHub(t, cap)
	defer func() { _ = hub.Close() }()

	if _, err := hub.NewSession(newChanSignaler()); err == nil {
		t.Fatal("expected NewSession to fail when the pipeline cannot start")
	}
	if hub.Sessions() != 0 {
		t.Fatalf("failed session left %d attached", hub.Sessions())
	}
}

// TestCandidatesBeforeOfferAreNotLost covers the race a browser always runs:
// it trickles candidates as soon as it sets its local description, which can
// reach the BMC before the offer does. pion rejects a candidate with no remote
// description, so without the queue those candidates disappear.
func TestCandidatesBeforeOfferAreNotLost(t *testing.T) {
	hub := newTestHub(t, newFakeCapturer())
	defer func() { _ = hub.Close() }()

	session, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()

	candidate := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2130706431 192.0.2.1 4444 typ host"}
	if err := session.HandleMessage(Message{Type: TypeCandidate, Candidate: &candidate}); err != nil {
		t.Fatalf("early candidate rejected: %v", err)
	}

	session.mu.Lock()
	queued := len(session.pending)
	session.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queued %d candidates, want 1", queued)
	}
}

func TestCandidateQueueIsBounded(t *testing.T) {
	hub := newTestHub(t, newFakeCapturer())
	defer func() { _ = hub.Close() }()

	session, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()

	candidate := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2130706431 192.0.2.1 4444 typ host"}
	for i := 0; i < maxPendingCandidates+3; i++ {
		if err := session.HandleMessage(Message{Type: TypeCandidate, Candidate: &candidate}); err != nil {
			t.Fatalf("candidate %d rejected: %v", i, err)
		}
	}

	session.mu.Lock()
	queued := len(session.pending)
	session.mu.Unlock()
	if queued != maxPendingCandidates {
		t.Fatalf("queued %d candidates, want the cap of %d", queued, maxPendingCandidates)
	}
}

// TestSessionStreamsToPeer negotiates against a real pion peer over loopback
// and checks that a frame written into the pipeline comes out the other end as
// RTP. It is the only test that exercises the whole path -- media engine,
// packetizer, DTLS/SRTP -- rather than the Hub's bookkeeping.
func TestSessionStreamsToPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("needs loopback UDP for ICE")
	}

	cap := newFakeCapturer()
	hub := newTestHub(t, cap)
	defer func() { _ = hub.Close() }()

	// The browser side. Default codecs so negotiation has to actually agree
	// on H.264 rather than being handed a single option.
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("register default codecs: %v", err)
	}
	browser, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("browser peer connection: %v", err)
	}
	defer func() { _ = browser.Close() }()

	if _, err := browser.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}

	gotRTP := make(chan struct{})
	var once sync.Once
	browser.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
			once.Do(func() { close(gotRTP) })
		}
	})

	connected := make(chan struct{})
	var connectOnce sync.Once
	browser.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateConnected {
			connectOnce.Do(func() { close(connected) })
		}
	})

	// Offer with candidates already gathered, so the test only has to
	// forward the BMC's trickled candidates back -- which is the direction
	// the Session actually implements.
	offer, err := browser.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(browser)
	if err := browser.SetLocalDescription(offer); err != nil {
		t.Fatalf("browser set local description: %v", err)
	}
	<-gathered

	sig := newChanSignaler()
	session, err := hub.NewSession(sig)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()

	err = session.HandleMessage(Message{Type: TypeOffer, SDP: browser.LocalDescription().SDP})
	if err != nil {
		t.Fatalf("handle offer: %v", err)
	}

	// Forward outbound signaling to the browser: the answer, then the
	// candidates the Session trickles. Errors go to a channel rather than
	// t.Errorf because this goroutine can outlive the test function.
	stop := make(chan struct{})
	defer close(stop)
	signalErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			case msg := <-sig.msgs:
				var err error
				switch msg.Type {
				case TypeAnswer:
					err = browser.SetRemoteDescription(webrtc.SessionDescription{
						Type: webrtc.SDPTypeAnswer,
						SDP:  msg.SDP,
					})
				case TypeCandidate:
					if msg.Candidate != nil {
						err = browser.AddICECandidate(*msg.Candidate)
					}
				default:
					// State pushes and errors are not part of what
					// this test drives.
				}
				if err != nil {
					select {
					case signalErr <- err:
					default:
					}
					return
				}
			}
		}
	}()

	// Frames written before the track binds are discarded by design, so wait
	// for the connection rather than racing it.
	select {
	case <-connected:
	case err := <-signalErr:
		t.Fatalf("signaling: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the peer connection to come up")
	}

	// One IDR is enough for the payloader to emit packets. The start code is
	// what makes it an Annex-B access unit -- without it pion finds no NAL
	// and sends nothing.
	frame := video.Frame{
		Data:     append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, make([]byte, 64)...),
		Keyframe: true,
	}

	send := time.NewTicker(20 * time.Millisecond)
	defer send.Stop()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-gotRTP:
			if _, _, keyframes := cap.counts(); keyframes == 0 {
				t.Error("no keyframe requested when the viewer connected")
			}
			if hub.FramesWritten() == 0 {
				t.Error("hub reported no frames written")
			}
			return
		case <-timeout:
			t.Fatal("no RTP reached the peer")
		case <-send.C:
			frame.PTS += 20 * time.Millisecond
			select {
			case cap.frames <- frame:
			default:
			}
		}
	}
}
