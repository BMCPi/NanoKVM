package rtc

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
)

// maxPendingCandidates bounds the ICE candidates buffered before the browser's
// offer arrives. A real negotiation trickles a handful; anything past this is a
// client that will not send an offer, and the queue is a memory leak an
// unauthenticated-looking endpoint should not offer.
const maxPendingCandidates = 64

// Deadlines that stop a session outliving the browser behind it.
//
// Both exist because a session is not free to leave lying around: attaching one
// starts the capture pipeline, and the pipeline runs until the last session
// detaches. A browser that opens the socket and then goes away -- a closed
// laptop, a dropped Wi-Fi link, a tab closed while the machine suspends --
// leaves the encoder running on a SoC that has one core to spare, indefinitely.
const (
	// negotiationTimeout bounds the wait for the browser's offer. Generous
	// on purpose: it is not a latency budget but a liveness one, and the
	// only thing it needs to beat is "never".
	negotiationTimeout = 30 * time.Second

	// disconnectGrace is how long a connected session may sit in
	// Disconnected before it is given up on. ICE reports that state for
	// ordinary transient loss and recovers from it by itself, so this has to
	// be long enough not to punish a brief blip -- but a closed browser tab
	// also passes through Disconnected, and does not always reach Failed.
	disconnectGrace = 20 * time.Second
)

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

	// Liveness deadlines. Both are nil when not armed, and both are stopped
	// by Close, so a session that ends normally leaves no timer behind.
	negotiate  *time.Timer
	disconnect *time.Timer

	done      chan struct{}
	closeOnce sync.Once
}

// armNegotiation starts the deadline for the browser's offer.
func (s *Session) armNegotiation() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.negotiate = time.AfterFunc(negotiationTimeout, func() {
		log.Warnf("rtc: no offer within %s, dropping session", negotiationTimeout)
		s.Close()
	})
}

// disarmNegotiation cancels that deadline, once an offer has arrived.
func (s *Session) disarmNegotiation() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.negotiate != nil {
		s.negotiate.Stop()
		s.negotiate = nil
	}
}

// armDisconnect starts the grace period for a session that has dropped to
// Disconnected. Repeated notifications do not extend it: the clock should run
// from when the link was first lost, not from the last time ICE said so.
func (s *Session) armDisconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.disconnect != nil {
		return
	}
	s.disconnect = time.AfterFunc(disconnectGrace, func() {
		log.Infof("rtc: still disconnected after %s, dropping session", disconnectGrace)
		s.Close()
	})
}

// clearDisconnect cancels the grace period, because the link came back.
func (s *Session) clearDisconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.disconnect != nil {
		s.disconnect.Stop()
		s.disconnect = nil
	}
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
			s.clearDisconnect()

			// Tell the browser where the capture stands, now that it is
			// listening. The push in NewSession happens before negotiation,
			// when the browser is still deciding whether it has a session at
			// all and discards it; after that, state only moves when the
			// capture state actually changes. A host that is simply sitting
			// there produces no change, so without this a session that
			// negotiated perfectly is never told anything and the UI cannot
			// tell "connected, waiting for the host" from "still connecting".
			s.sendState(h.State())

			// The console runs a long GOP because its picture barely
			// changes, so a viewer that joins between keyframes would
			// otherwise see nothing until the next scheduled one.
			if err := h.cap.RequestKeyframe(); err != nil &&
				!errors.Is(err, video.ErrNotSupported) {
				log.Warnf("rtc: keyframe on connect: %s", err)
			}

		case webrtc.PeerConnectionStateDisconnected:
			// Not fatal on its own: ICE reports this for transient loss and
			// recovers by itself, so tearing down here would turn a blip
			// into a reconnect. But it is also what a closed browser tab
			// looks like on the way to failed, and a session that never
			// arrives at either leaves the capture pipeline running for a
			// viewer that is gone. So it gets a deadline rather than a pass.
			s.armDisconnect()

		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.Close()

		case webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting:
			// Transient states on the way up (or back down through
			// Disconnected/Failed); there is nothing to do here beyond the
			// debug log above.

		default:
			// webrtc.PeerConnectionStateUnknown. pion's updateConnectionState
			// (peerconnection.go) always starts its local from
			// PeerConnectionStateNew and only ever assigns one of the named
			// states above, so this never fires with the pion version this
			// package pins -- but an unrecognized state is not one this state
			// machine can reason about, so if a future pion release ever adds
			// one, close rather than silently leaving the session (and the
			// capture pipeline behind it) attached under a status nothing here
			// understands.
			s.Close()
		}
	})

	// Give the browser the current signal state before negotiation, so it
	// can show "no signal" immediately instead of an indefinite spinner.
	s.sendState(h.State())

	// And bound the wait for an offer. Until one arrives this session is
	// holding the capture pipeline up on behalf of a browser that has not
	// asked for anything, which a half-open socket can do indefinitely.
	s.armNegotiation()

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

		// Timers first: both fire into Close, and closeOnce makes that
		// harmless, but a session that has ended should not be holding a
		// pending wakeup either.
		s.disarmNegotiation()
		s.clearDisconnect()

		s.hub.detach(s)
		if s.pc != nil {
			if err := s.pc.Close(); err != nil {
				log.Debugf("rtc: close peer connection: %s", err)
			}
		}
	})
}

func (s *Session) handleOffer(sdp string) error {
	// The browser has asked for something, so it is no longer a socket that
	// might never negotiate. Cancel before doing the work rather than after:
	// a renegotiation on a live session must not re-arm a deadline that would
	// then fire under a perfectly healthy stream.
	s.disarmNegotiation()

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
