package rtc

import (
	"errors"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
)

// maxPendingCandidates bounds the ICE candidates buffered before the browser's
// offer arrives. A real negotiation trickles a handful; anything past this is a
// client that will not send an offer, and the queue is a memory leak an
// unauthenticated-looking endpoint should not offer.
const maxPendingCandidates = 64

// Session is one browser's peer connection to the console.
//
// It is driven from outside: the transport hands inbound signaling to
// HandleMessage and the session writes outbound signaling through its Signaler.
// Nothing here reads from a socket, which is what lets the WebSocket handler
// own its own read loop and lets tests drive a session with no network at all.
type Session struct {
	hub *Hub
	sig Signaler
	pc  *webrtc.PeerConnection

	mu         sync.Mutex
	pending    []webrtc.ICECandidateInit
	haveRemote bool

	done      chan struct{}
	closeOnce sync.Once
}

// NewSession creates a peer connection, binds it to the Hub's track and
// attaches it to the Hub -- which starts the capture pipeline if this is the
// first viewer.
//
// The returned Session has not negotiated yet; it is waiting for the browser's
// offer. Callers must Close it when the transport goes away, or the pipeline
// keeps running for a viewer that is no longer there.
func (h *Hub) NewSession(sig Signaler) (*Session, error) {
	if sig == nil {
		return nil, errors.New("rtc: nil signaler")
	}

	s := &Session{hub: h, sig: sig, done: make(chan struct{})}

	if err := h.attach(s); err != nil {
		return nil, err
	}

	pc, err := h.api.NewPeerConnection(h.cfg)
	if err != nil {
		h.detach(s)
		return nil, fmt.Errorf("rtc: new peer connection: %w", err)
	}
	s.pc = pc

	sender, err := pc.AddTrack(h.track)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("rtc: add track: %w", err)
	}
	go h.readRTCP(sender)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		// A nil candidate is end-of-gathering. Forwarding it as an empty
		// candidate message lets the browser stop waiting for more,
		// which shortens connection setup on a network where some
		// candidate types never resolve.
		if c == nil {
			_ = s.send(Message{Type: TypeCandidate})
			return
		}
		init := c.ToJSON()
		_ = s.send(Message{Type: TypeCandidate, Candidate: &init})
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Debugf("rtc: connection state %s", st)
		switch st {
		case webrtc.PeerConnectionStateConnected:
			// The console runs a long GOP because its picture barely
			// changes, so a viewer that joins between keyframes would
			// otherwise see nothing until the next scheduled one.
			if err := h.cap.RequestKeyframe(); err != nil &&
				!errors.Is(err, video.ErrNotSupported) {
				log.Warnf("rtc: keyframe on connect: %s", err)
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.Close()
		default:
			// New, Connecting and Disconnected are all states to sit
			// through. Disconnected especially: ICE reports it on
			// transient loss and recovers on its own, so tearing down
			// there would turn a blip in the network into a reconnect.
		}
	})

	// Give the browser the current signal state before negotiation, so it
	// can show "no signal" immediately instead of an indefinite spinner.
	s.sendState(h.State())

	return s, nil
}

// HandleMessage processes one inbound signaling message.
func (s *Session) HandleMessage(m Message) error {
	switch m.Type {
	case TypeOffer:
		return s.handleOffer(m.SDP)
	case TypeCandidate:
		return s.handleCandidate(m.Candidate)
	default:
		return fmt.Errorf("rtc: unexpected inbound message type %q", m.Type)
	}
}

// Done is closed when the session ends, from either side.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close tears the session down and detaches it from the Hub, stopping the
// capture pipeline if it was the last viewer. It is safe to call repeatedly
// and from any goroutine.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.hub.detach(s)
		if s.pc != nil {
			if err := s.pc.Close(); err != nil {
				log.Debugf("rtc: close peer connection: %s", err)
			}
		}
	})
}

func (s *Session) handleOffer(sdp string) error {
	err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	})
	if err != nil {
		return fmt.Errorf("rtc: set remote description: %w", err)
	}
	s.flushCandidates()

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("rtc: create answer: %w", err)
	}
	// Starts ICE gathering, so candidates begin trickling out of
	// OnICECandidate from here.
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("rtc: set local description: %w", err)
	}

	return s.send(Message{Type: TypeAnswer, SDP: answer.SDP})
}

// handleCandidate adds a trickled candidate, queueing it if the offer has not
// arrived yet.
//
// The queue is needed because browsers start trickling as soon as they call
// setLocalDescription, which races the offer down a separate WebSocket write;
// pion rejects a candidate outright when there is no remote description, so
// without this the first few candidates are simply lost and connection setup
// falls back to whatever arrives later.
func (s *Session) handleCandidate(c *webrtc.ICECandidateInit) error {
	if c == nil {
		// End-of-candidates from the browser. pion infers the same thing
		// from gathering state, so there is nothing to add.
		return nil
	}

	s.mu.Lock()
	if !s.haveRemote {
		if len(s.pending) >= maxPendingCandidates {
			s.mu.Unlock()
			log.Warnf("rtc: dropping ICE candidate, %d already queued with no offer",
				maxPendingCandidates)
			return nil
		}
		s.pending = append(s.pending, *c)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	if err := s.pc.AddICECandidate(*c); err != nil {
		return fmt.Errorf("rtc: add ice candidate: %w", err)
	}
	return nil
}

// flushCandidates drains the queue built up before the remote description was
// set. A candidate that fails to add is logged rather than returned: one bad
// candidate should not fail the whole negotiation when the others may connect.
func (s *Session) flushCandidates() {
	s.mu.Lock()
	s.haveRemote = true
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()

	for _, c := range pending {
		if err := s.pc.AddICECandidate(c); err != nil {
			log.Warnf("rtc: queued ice candidate rejected: %s", err)
		}
	}
}

// sendState pushes capture state, ignoring transport failures: the state push
// is advisory, and a dead transport is already being reported by the read loop.
func (s *Session) sendState(st video.State) {
	_ = s.send(Message{Type: TypeState, State: &st})
}

func (s *Session) send(m Message) error {
	select {
	case <-s.done:
		return ErrClosed
	default:
	}
	return s.sig.Send(m)
}
