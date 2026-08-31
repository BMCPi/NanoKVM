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

func (f *fakeCapturer) Start(_ video.Config) error {
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

func newTestHub(t *testing.T, capturer video.Capturer) *Hub {
	t.Helper()
	hub, err := NewHub(capturer, Options{Capture: video.Config{Codec: video.CodecH264}})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	return hub
}

// shortenLinger collapses the post-viewer grace period so a test does not have
// to wait out five real seconds to observe the pipeline stopping.
func shortenLinger(t *testing.T) {
	t.Helper()
	prev := pipelineLinger
	pipelineLinger = time.Millisecond
	t.Cleanup(func() { pipelineLinger = prev })
}

// waitForStops blocks until the capturer has been stopped want times. The stop
// is deferred by the linger and runs on a timer goroutine, so it cannot be
// asserted synchronously the way the start can.
func waitForStops(t *testing.T, capturer *fakeCapturer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, stops, _ := capturer.counts(); stops >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, stops, _ := capturer.counts()
	t.Fatalf("pipeline stopped %d times, want %d", stops, want)
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
	shortenLinger(t)

	capturer := newFakeCapturer()
	hub := newTestHub(t, capturer)

	first, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if starts, stops, _ := capturer.counts(); starts != 1 || stops != 0 {
		t.Fatalf("after first session: starts=%d stops=%d, want 1/0", starts, stops)
	}

	second, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	if starts, stops, _ := capturer.counts(); starts != 1 || stops != 0 {
		t.Fatalf("second session restarted the pipeline: starts=%d stops=%d, want 1/0", starts, stops)
	}

	first.Close()
	if starts, stops, _ := capturer.counts(); starts != 1 || stops != 0 {
		t.Fatalf("pipeline stopped while a viewer remained: starts=%d stops=%d, want 1/0", starts, stops)
	}

	// The last viewer leaving stops it, but only after the linger -- a page
	// reload is a disconnect immediately followed by a connect, and tearing
	// the capture chain down for that is both slow and where this hardware
	// breaks.
	second.Close()
	waitForStops(t, capturer, 1)
	if starts, _, _ := capturer.counts(); starts != 1 {
		t.Fatalf("pipeline restarted while idle: starts=%d, want 1", starts)
	}

	// A later viewer starts it again rather than finding a dead hub.
	third, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("third session: %v", err)
	}
	if starts, _, _ := capturer.counts(); starts != 2 {
		t.Fatalf("pipeline did not restart for a new viewer: starts=%d, want 2", starts)
	}
	third.Close()

	if err := hub.Close(); err != nil {
		t.Fatalf("hub close: %v", err)
	}
}

func TestSessionRefusedWhenPipelineWillNotStart(t *testing.T) {
	capturer := newFakeCapturer()
	capturer.startErr = errors.New("no capture hardware")
	hub := newTestHub(t, capturer)
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

	capturer := newFakeCapturer()
	hub := newTestHub(t, capturer)
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
			if _, _, keyframes := capturer.counts(); keyframes == 0 {
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
			case capturer.frames <- frame:
			default:
			}
		}
	}
}

// A session holds the capture pipeline up for as long as it exists, so one
// whose browser never negotiates is not merely idle -- it keeps the encoder
// running on a one-core SoC for a viewer that is not there. The deadline is
// what stops a half-open socket doing that indefinitely.
func TestNegotiationDeadlineIsArmedUntilAnOfferArrives(t *testing.T) {
	hub := newTestHub(t, newFakeCapturer())
	defer func() { _ = hub.Close() }()

	session, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()

	session.mu.Lock()
	armed := session.negotiate != nil
	session.mu.Unlock()
	if !armed {
		t.Fatal("no negotiation deadline armed; a socket that never offers would hold the pipeline forever")
	}

	// An offer is the browser asking for something, which is what makes the
	// session legitimate. Renegotiation later must not re-arm the deadline.
	session.disarmNegotiation()

	session.mu.Lock()
	stillArmed := session.negotiate != nil
	session.mu.Unlock()
	if stillArmed {
		t.Error("negotiation deadline survived the offer; it could fire under a live stream")
	}
}

// Closing must leave nothing pending. Both timers call Close, which closeOnce
// makes harmless, but a session that has ended should not be holding wakeups.
func TestCloseDisarmsBothDeadlines(t *testing.T) {
	hub := newTestHub(t, newFakeCapturer())
	defer func() { _ = hub.Close() }()

	session, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	session.armDisconnect()
	session.Close()

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.negotiate != nil {
		t.Error("negotiation deadline still armed after Close")
	}
	if session.disconnect != nil {
		t.Error("disconnect grace still armed after Close")
	}
}

// ICE reports Disconnected repeatedly while a link is down. The grace period
// has to run from when it was first lost -- restarting it on every
// notification would let a permanently dead session renew itself forever.
func TestDisconnectGraceDoesNotRestartOnRepeatedNotifications(t *testing.T) {
	hub := newTestHub(t, newFakeCapturer())
	defer func() { _ = hub.Close() }()

	session, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()

	session.armDisconnect()
	session.mu.Lock()
	first := session.disconnect
	session.mu.Unlock()
	if first == nil {
		t.Fatal("disconnect grace was not armed")
	}

	session.armDisconnect()
	session.mu.Lock()
	second := session.disconnect
	session.mu.Unlock()
	if second != first {
		t.Error("a second Disconnected replaced the timer, restarting the grace period")
	}

	// Recovery cancels it: the link came back, so the session is healthy.
	session.clearDisconnect()
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.disconnect != nil {
		t.Error("disconnect grace survived reconnection")
	}
}

// A page reload is a disconnect immediately followed by a connect. Rebuilding
// the capture chain for that is slow -- seconds on this SoC -- and it is also
// how a reload could hang outright: the stop runs under the Hub's lock and
// waits for the capture supervisor, so the reconnecting browser's attach queues
// behind a teardown that may be part-way through a bring-up. NewSession does
// not return, no answer is sent, and the page sits on "Connecting..." until the
// teardown finishes, which for a wedged media driver is never.
func TestReloadReusesTheRunningPipeline(t *testing.T) {
	shortenLinger(t)

	capturer := newFakeCapturer()
	hub := newTestHub(t, capturer)
	defer func() { _ = hub.Close() }()

	first, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("first session: %v", err)
	}

	// The reload: gone and back before the linger expires.
	first.Close()
	second, err := hub.NewSession(newChanSignaler())
	if err != nil {
		t.Fatalf("session after reload: %v", err)
	}
	defer second.Close()

	starts, stops, _ := capturer.counts()
	if stops != 0 {
		t.Errorf("reload tore the pipeline down: stops=%d, want 0", stops)
	}
	if starts != 1 {
		t.Errorf("reload rebuilt the pipeline: starts=%d, want 1", starts)
	}

	// And the reprieve is real rather than merely deferred: the pipeline is
	// still up well after the linger would have expired, because a viewer
	// arrived in the meantime.
	time.Sleep(20 * time.Millisecond)
	if _, stops, _ := capturer.counts(); stops != 0 {
		t.Errorf("pipeline stopped under an attached viewer: stops=%d, want 0", stops)
	}
}
