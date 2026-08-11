package rtc

import (
	"github.com/pion/webrtc/v4"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
)

// MessageType tags a signaling message.
type MessageType string

const (
	// TypeOffer carries an SDP offer from the browser.
	//
	// The browser offers and the BMC answers, rather than the other way
	// round, because the browser is the side that knows what it can decode
	// and the side that will later open a data channel for keyboard and
	// mouse. Answering also keeps the BMC from having to guess at receiver
	// capabilities it would then have to renegotiate away from.
	TypeOffer MessageType = "offer"

	// TypeAnswer carries the BMC's SDP answer.
	TypeAnswer MessageType = "answer"

	// TypeCandidate carries one trickled ICE candidate, in either
	// direction. An empty Candidate means end-of-candidates.
	TypeCandidate MessageType = "candidate"

	// TypeState pushes capture state (signal present, resolution, fps) to
	// the browser. Sent unsolicited whenever the pipeline reports a change,
	// so the UI can show "no signal" without polling a REST endpoint.
	TypeState MessageType = "state"

	// TypeError reports a failure the browser should surface. The session
	// may or may not survive it; a closed socket is the authoritative
	// signal that it did not.
	TypeError MessageType = "error"
)

// Message is one signaling message. It is deliberately one flat struct with
// optional fields rather than a discriminated union: the whole protocol is
// five message types over a WebSocket the browser and this package both
// implement, and a tagged envelope with a nested json.RawMessage would cost a
// second unmarshal per message to buy nothing.
type Message struct {
	Type MessageType `json:"type"`

	// SDP is set on offer and answer.
	SDP string `json:"sdp,omitempty"`

	// Candidate is set on candidate messages.
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`

	// State is set on state messages.
	State *video.State `json:"state,omitempty"`

	// Error is set on error messages.
	Error string `json:"error,omitempty"`
}

// Signaler is the transport a Session exchanges messages over.
//
// It exists so the session logic does not depend on WebSockets: the API layer
// supplies an implementation wrapping gorilla/websocket, and tests supply a
// channel. Send may be called from several goroutines (the ICE agent, the
// state watcher, the request handler), so implementations must serialise
// writes themselves -- a WebSocket connection permits only one writer at a
// time.
type Signaler interface {
	Send(Message) error
}
